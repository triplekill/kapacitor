package smtp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/gomail.v2"
)

var ErrNoRecipients = errors.New("not sending email, no recipients defined")

type Service struct {
	mu      sync.Mutex
	config  atomic.Value
	mail    chan *gomail.Message
	updates chan bool
	logger  *log.Logger
	wg      sync.WaitGroup
	opened  bool
}

func NewService(c Config, l *log.Logger) *Service {
	s := &Service{
		updates: make(chan bool),
		logger:  l,
	}
	s.config.Store(c)
	return s
}

func (s *Service) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened {
		return nil
	}
	s.opened = true

	s.logger.Println("I! Starting SMTP service")

	s.mail = make(chan *gomail.Message)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runMailer()
	}()

	return nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.opened {
		return nil
	}
	s.opened = false

	s.logger.Println("I! Closing SMTP service")

	close(s.mail)
	s.wg.Wait()

	return nil
}

func (s *Service) loadConfig() Config {
	return s.config.Load().(Config)
}

func (s *Service) Update(newConfig []interface{}) error {
	if l := len(newConfig); l != 1 {
		return fmt.Errorf("expected only one new config object, got %d", l)
	}
	if c, ok := newConfig[0].(Config); !ok {
		return fmt.Errorf("expected config object to be of type %T, got %T", c, newConfig[0])
	} else {
		s.config.Store(c)
		s.mu.Lock()
		opened := s.opened
		s.mu.Unlock()
		if opened {
			// Signal to create new dialer
			s.updates <- true
		}
	}
	return nil
}

func (s *Service) Global() bool {
	c := s.loadConfig()
	return c.Global
}

func (s *Service) StateChangesOnly() bool {
	c := s.loadConfig()
	return c.StateChangesOnly
}

func (s *Service) dialer() (d *gomail.Dialer, idleTimeout time.Duration) {
	c := s.loadConfig()
	if c.Username == "" {
		d = &gomail.Dialer{Host: c.Host, Port: c.Port}
	} else {
		d = gomail.NewPlainDialer(c.Host, c.Port, c.Username, c.Password)
	}
	if c.NoVerify {
		d.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}
	idleTimeout = time.Duration(c.IdleTimeout)
	return
}

func (s *Service) runMailer() {
	var idleTimeout time.Duration
	var d *gomail.Dialer
	d, idleTimeout = s.dialer()

	var conn gomail.SendCloser
	var err error
	open := false
	done := false
	for !done {
		timer := time.NewTimer(idleTimeout)
		select {
		case <-s.updates:
			// Close old connection
			if conn != nil {
				if err := conn.Close(); err != nil {
					s.logger.Println("E! error closing connection to old SMTP server:", err)
				}
				conn = nil
			}
			// Create new dialer
			d, idleTimeout = s.dialer()
			open = false
		case m, ok := <-s.mail:
			if !ok {
				done = true
				break
			}
			if !open {
				if conn, err = d.Dial(); err != nil {
					s.logger.Println("E! error connecting to SMTP server", err)
					break
				}
				open = true
			}
			if err := gomail.Send(conn, m); err != nil {
				s.logger.Println("E!", err)
			}
		// Close the connection to the SMTP server if no email was sent in
		// the last IdleTimeout duration.
		case <-timer.C:
			if open {
				if err := conn.Close(); err != nil {
					s.logger.Println("E! error closing connection to SMTP server:", err)
				}
				open = false
			}
		}
		timer.Stop()
	}
}

func (s *Service) SendMail(to []string, subject, body string) error {
	m, err := s.prepareMessge(to, subject, body)
	if err != nil {
		return err
	}
	s.mail <- m
	return nil
}

func (s *Service) prepareMessge(to []string, subject, body string) (*gomail.Message, error) {
	c := s.loadConfig()
	if !c.Enabled {
		return nil, errors.New("service not enabled")
	}
	if len(to) == 0 {
		to = c.To
	}
	if len(to) == 0 {
		return nil, ErrNoRecipients
	}
	m := gomail.NewMessage()
	m.SetHeader("From", c.From)
	m.SetHeader("To", to...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/html", body)
	return m, nil
}
