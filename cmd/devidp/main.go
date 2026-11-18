// Command devidp is a stand-in OpenID Connect identity provider for local
// development and tests (for example the Helm chart test on a throwaway
// cluster). Anyone who can reach it can sign in as any listed user: it has no
// passwords. NEVER run it anywhere that matters.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/getplinth/plinth/internal/auth/authtest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "address to listen on")
	issuer := flag.String("issuer", "", "address clients use to reach it (default: http://<addr>)")
	audience := flag.String("audience", "plinth-ui", "client id that tokens are issued for")
	adminGroup := flag.String("admin-group", "plinth-admins", "group that makes a user an administrator")
	flag.Parse()

	p, err := authtest.NewAt(*audience, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if *issuer != "" {
		p.SetPublicURL(*issuer)
	}
	p.EnableLogin([]authtest.User{
		{Sub: "alice", Email: "alice@example.test", Name: "Alice (developer)", Groups: []string{"developers"}},
		{Sub: "bob", Email: "bob@example.test", Name: "Bob (developer)", Groups: []string{"developers"}},
		{Sub: "root", Email: "root@example.test", Name: "Root (platform admin)", Groups: []string{*adminGroup}},
		{Sub: "ada", Email: "ada@example.test", Name: "Ada (platform admin)", Groups: []string{*adminGroup}},
	})
	fmt.Printf("devidp (NO PASSWORDS, development only) listening on %s, issuer %s\n", *addr, p.Issuer())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	p.Close()
}
