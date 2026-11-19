// Package notify delivers notifications about invitations and requests by
// e-mail and webhook. Events are queued by the store in the same transaction as
// the change they report; a Dispatcher sends them with retries.
//
// Messages say what happened and link to the page. They never contain
// credentials, error text or anything a provisioning step produced.
package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/getplinth/plinth/internal/store"
)

// Message is a rendered notification.
type Message struct {
	Subject string
	Body    string // plain text
	Text    string // one line, for chat webhooks
}

// clean removes line breaks and control characters from text that came from a
// person (names, notes, e-mail addresses) before it goes into a header or a line.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func str(d map[string]any, k string) string { s, _ := d[k].(string); return clean(s) }

func projects(d map[string]any) string {
	list, _ := d["projects"].([]any)
	names := make([]string, 0, len(list))
	for _, p := range list {
		if s, ok := p.(string); ok {
			names = append(names, clean(s))
		}
	}
	switch len(names) {
	case 0:
		return "no projects"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Render turns an event into a message. base is the public address of Plinth
// (no trailing slash) or empty, in which case no links are included.
func Render(event string, d map[string]any, base string) Message {
	link := func(path string) string {
		if base == "" {
			return ""
		}
		return "\n\n" + base + path
	}
	team, who := str(d, "tenant"), str(d, "requested_by")
	req := "/requests/" + str(d, "request_id")
	var m Message
	switch event {
	case store.EventInvitation:
		role := str(d, "role")
		exp := ""
		if t, err := time.Parse(time.RFC3339, str(d, "expires_at")); err == nil {
			exp = " The invitation expires on " + t.UTC().Format("2 January 2006") + "."
		}
		m.Subject = fmt.Sprintf("%s invited you to the team %s on Plinth", str(d, "invited_by"), team)
		m.Body = fmt.Sprintf("%s invited you to join the team %q on Plinth as %s.\n\nSign in with this e-mail address (%s) and you join automatically.%s%s",
			str(d, "invited_by"), team, article(role), str(d, "email"), exp, link("/"))
		m.Text = fmt.Sprintf("%s invited %s to the team %s as %s", str(d, "invited_by"), str(d, "email"), team, role)
	case store.EventAwaitingApproval:
		m.Subject = fmt.Sprintf("Approval needed: %s for team %s", projects(d), team)
		m.Body = fmt.Sprintf("%s asked for %s in the team %q using the blueprint %q. Nothing is set up until another administrator approves it.%s",
			who, projects(d), team, str(d, "blueprint"), link("/admin/approvals"))
		m.Text = fmt.Sprintf("%s needs approval: %s for team %s (blueprint %s)", who, projects(d), team, str(d, "blueprint"))
	case store.EventApproved:
		m.Subject = fmt.Sprintf("Approved: %s for team %s", projects(d), team)
		m.Body = fmt.Sprintf("%s approved your request for %s in the team %q. It is being set up now.%s", str(d, "decided_by"), projects(d), team, link(req))
		m.Text = fmt.Sprintf("%s approved the request for %s in team %s", str(d, "decided_by"), projects(d), team)
	case store.EventRejected:
		reason := ""
		if n := str(d, "note"); n != "" {
			reason = fmt.Sprintf("\n\nTheir reason: %s", n)
		}
		m.Subject = fmt.Sprintf("Rejected: %s for team %s", projects(d), team)
		m.Body = fmt.Sprintf("%s rejected your request for %s in the team %q. Nothing was set up.%s\n\nChange the request and submit it again.%s",
			str(d, "decided_by"), projects(d), team, reason, link(req))
		m.Text = fmt.Sprintf("%s rejected the request for %s in team %s", str(d, "decided_by"), projects(d), team)
	case store.EventSucceeded:
		m.Subject = fmt.Sprintf("Ready: %s for team %s", projects(d), team)
		m.Body = fmt.Sprintf("Your request for %s in the team %q is done. The details, and how to get any credentials, are on the request page.%s", projects(d), team, link(req))
		m.Text = fmt.Sprintf("Request for %s in team %s is done (requested by %s)", projects(d), team, who)
	case store.EventFailed:
		step := ""
		if s := str(d, "failed_step"); s != "" {
			step = fmt.Sprintf(" The step %q did not finish.", s)
		}
		m.Subject = fmt.Sprintf("Failed: %s for team %s", projects(d), team)
		m.Body = fmt.Sprintf("Your request for %s in the team %q stopped.%s Finished steps are kept; open the request to see the reason and retry.%s", projects(d), team, step, link(req))
		m.Text = fmt.Sprintf("Request for %s in team %s failed%s (requested by %s)", projects(d), team, strings.TrimSuffix(strings.ReplaceAll(step, `"`, ""), "."), who)
	case store.EventAbandoned:
		m.Subject = fmt.Sprintf("Given up: %s for team %s", projects(d), team)
		m.Body = fmt.Sprintf("%s gave up the failed request for %s in the team %q. Its names are free again.%s", str(d, "decided_by"), projects(d), team, link(req))
		m.Text = fmt.Sprintf("%s gave up the failed request for %s in team %s", str(d, "decided_by"), projects(d), team)
	case store.EventTest:
		m.Subject = "Plinth test notification"
		m.Body = fmt.Sprintf("This is a test message from Plinth, sent by %s. If you can read it, notifications work.", str(d, "by"))
		m.Text = "Plinth test notification, sent by " + str(d, "by")
	default:
		m.Subject = "Plinth: " + clean(event)
		m.Body = "Something happened in Plinth: " + clean(event) + "." + link("/")
		m.Text = "Plinth: " + clean(event)
	}
	return m
}

func article(role string) string {
	if role == "owner" {
		return "an owner"
	}
	return "a member"
}
