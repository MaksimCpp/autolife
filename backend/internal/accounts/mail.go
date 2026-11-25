package accounts

import (
	"context"
	"errors"
	"net"
	"net/smtp"
	"time"
)

type devMailer struct {
	address string
	origin  string
}

func (m devMailer) send(ctx context.Context, email, action, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", m.address)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	host, _, _ := net.SplitHostPort(m.address)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	const sender = "accounts@example.test"
	if err := client.Mail(sender); err != nil {
		return err
	}
	if err := client.Rcpt(email); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	link := m.origin + "/ru/account/" + action + "#token=" + token
	if _, err := writer.Write([]byte("From: " + sender + "\r\nTo: " + email + "\r\nSubject: AutoLife account " + action +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nAutoLife\r\n" + link + "\r\n")); err != nil {
		return errors.New("SMTP message write failed")
	}
	return writer.Close()
}
