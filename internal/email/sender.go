package email

import (
	"fmt"
	"mime"
	"net/smtp"
	"strings"
)

// Sender sends newsletter emails via SMTP.
type Sender struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
}

// Send sends an email with both HTML and plain-text bodies.
func (s *Sender) Send(to, subject, htmlBody, textBody string) error {
	if s.Host == "" {
		return fmt.Errorf("SMTP host not configured")
	}

	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)

	from := s.From
	if s.FromName != "" {
		from = fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", s.FromName), s.From)
	}

	boundary := "herald-newsletter-boundary"
	var msg strings.Builder
	msg.WriteString("From: " + from + "\r\n")
	msg.WriteString("To: " + to + "\r\n")
	msg.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	msg.WriteString("\r\n")

	// Plain text part.
	msg.WriteString("--" + boundary + "\r\n")
	msg.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	msg.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	msg.WriteString("\r\n")
	if textBody != "" {
		msg.WriteString(textBody)
	} else {
		msg.WriteString("This newsletter is best viewed in an HTML email client.")
	}
	msg.WriteString("\r\n")

	// HTML part wrapped in a minimal email template.
	msg.WriteString("--" + boundary + "\r\n")
	msg.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n")
	msg.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(wrapHTML(subject, htmlBody))
	msg.WriteString("\r\n")

	msg.WriteString("--" + boundary + "--\r\n")

	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, s.Host)
	}

	return smtp.SendMail(addr, auth, s.From, []string{to}, []byte(msg.String()))
}

// wrapHTML wraps newsletter body HTML in a minimal inline-CSS email template.
func wrapHTML(title, body string) string {
	return `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>` + title + `</title></head>
<body style="margin:0;padding:0;background:#f5f5f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;">
<table width="100%" cellpadding="0" cellspacing="0" style="background:#f5f5f5;">
<tr><td align="center" style="padding:20px;">
<table width="600" cellpadding="0" cellspacing="0" style="background:#ffffff;border-radius:4px;overflow:hidden;">
<tr><td style="padding:30px;font-size:16px;line-height:1.6;color:#333;">
` + body + `
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`
}
