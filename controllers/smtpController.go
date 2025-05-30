package controllers

import (
	"bytes"
	"crypto/tls"
	"net/smtp"
	"os"

	"github.com/jordan-wright/email"
)

func SendMail(to string, subject string, text string, files []struct {
	Name string
	Data []byte
}) error {
	password, _ := os.LookupEnv("SMTP_PASSWORD")

	e := email.NewEmail()
	e.From = "Финтехник <noreply@fintechnik.online>"
	e.To = []string{to}
	e.Subject = subject
	e.Text = []byte(text)

	if files != nil {
		for _, f := range files {
			_, err := e.Attach(bytes.NewReader(f.Data), f.Name, "application/zip")
			if err != nil {
				return err
			}
		}
	}

	err := e.SendWithTLS("smtp.mail.ru:465",
		smtp.PlainAuth("", "noreply@fintechnik.online", password, "smtp.mail.ru"),
		&tls.Config{ServerName: "smtp.mail.ru"})
	if err != nil {
		return err
	}

	return nil
}
