package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a small SMTP server for tests: EHLO, STARTTLS, AUTH PLAIN, MAIL,
// RCPT, DATA, QUIT. It records what it receives and can be told to misbehave.
type fakeSMTP struct {
	ln       net.Listener
	cert     tls.Certificate
	roots    *x509.CertPool
	implicit bool // TLS from the first byte

	NoSTARTTLS bool      // do not offer STARTTLS
	RcptReply  string    // reply to RCPT, default "250 ok"
	DataReply  string    // reply after the message, default "250 queued"
	AuthReply  string    // reply to AUTH, default "235 ok"
	Want       [2]string // required username and password; empty accepts any

	mu       sync.Mutex
	Messages []smtpMessage
	Auths    []string
	SawTLS   bool
	Plain    bool // received AUTH before any encryption
}

type smtpMessage struct{ From, To, Data string }

func newFakeSMTP(t *testing.T, implicit bool) *fakeSMTP {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	leaf, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots: roots, implicit: implicit}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) client() *tls.Config {
	return &tls.Config{RootCAs: s.roots, ServerName: "localhost"}
}

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	encrypted := false
	if s.implicit {
		c = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
		encrypted = true
		s.mu.Lock()
		s.SawTLS = true
		s.mu.Unlock()
	}
	r, w := bufio.NewReader(c), func(f string, a ...any) { fmt.Fprintf(c, f+"\r\n", a...) }
	w("220 localhost ready")
	var from, to string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			fmt.Fprintf(c, "250-localhost\r\n")
			if !encrypted && !s.NoSTARTTLS {
				fmt.Fprintf(c, "250-STARTTLS\r\n")
			}
			w("250 AUTH PLAIN")
		case cmd == "STARTTLS":
			w("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if err := tc.Handshake(); err != nil {
				return
			}
			c, r = tc, bufio.NewReader(tc)
			w = func(f string, a ...any) { fmt.Fprintf(tc, f+"\r\n", a...) }
			encrypted = true
			s.mu.Lock()
			s.SawTLS = true
			s.mu.Unlock()
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			parts := strings.Split(string(raw), "\x00")
			s.mu.Lock()
			s.Auths = append(s.Auths, strings.Join(parts[1:], ":"))
			if !encrypted {
				s.Plain = true
			}
			s.mu.Unlock()
			if s.Want[0] != "" && (len(parts) != 3 || parts[1] != s.Want[0] || parts[2] != s.Want[1]) {
				w("535 5.7.8 bad credentials")
			} else if s.AuthReply != "" {
				w("%s", s.AuthReply)
			} else {
				w("235 ok")
			}
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			from = strings.Trim(line[len("MAIL FROM:"):], "<> ")
			w("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			to = strings.Trim(line[len("RCPT TO:"):], "<> ")
			if s.RcptReply != "" {
				w("%s", s.RcptReply)
			} else {
				w("250 ok")
			}
		case cmd == "DATA":
			w("354 end with <CRLF>.<CRLF>")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			reply := "250 queued"
			if s.DataReply != "" {
				reply = s.DataReply
			}
			if strings.HasPrefix(reply, "250") {
				s.mu.Lock()
				s.Messages = append(s.Messages, smtpMessage{From: from, To: to, Data: b.String()})
				s.mu.Unlock()
			}
			w("%s", reply)
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("502 not implemented")
		}
	}
}

func (s *fakeSMTP) got() []smtpMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]smtpMessage(nil), s.Messages...)
}
