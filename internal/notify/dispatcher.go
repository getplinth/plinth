package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/getplinth/plinth/internal/store"
)

// Dispatcher sends queued notifications. Run one per replica: they share the
// queue safely, and a notification is picked up by exactly one at a time.
type Dispatcher struct {
	Store   *store.Store
	Log     *slog.Logger
	Email   *SMTP    // nil: e-mail is not configured
	Webhook *Webhook // nil: no webhook
	// PublicURL is the address people use for Plinth, for links in messages.
	PublicURL string

	Poll        time.Duration // how often to look for work, default 2s
	Batch       int           // notifications per round, default 20
	Lease       time.Duration // how long a claimed notification is reserved, default 2m
	MaxAttempts int           // default 8
	Backoff     time.Duration // first retry delay, doubled each time up to MaxBackoff, default 30s
	MaxBackoff  time.Duration // default 1h
}

func (d *Dispatcher) defaults() {
	if d.Poll == 0 {
		d.Poll = 2 * time.Second
	}
	if d.Batch == 0 {
		d.Batch = 20
	}
	if d.Lease == 0 {
		d.Lease = 2 * time.Minute
	}
	if d.MaxAttempts == 0 {
		d.MaxAttempts = 8
	}
	if d.Backoff == 0 {
		d.Backoff = 30 * time.Second
	}
	if d.MaxBackoff == 0 {
		d.MaxBackoff = time.Hour
	}
}

// Run delivers until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	d.defaults()
	tick := time.NewTicker(d.Poll)
	defer tick.Stop()
	purge := time.NewTicker(time.Hour)
	defer purge.Stop()
	for {
		for d.Once(ctx) == d.Batch && ctx.Err() == nil { // keep going while there is a backlog
		}
		select {
		case <-ctx.Done():
			return
		case <-purge.C:
			if err := d.Store.PurgeNotifications(ctx, 7*24*time.Hour, 30*24*time.Hour); err != nil {
				d.Log.Warn("could not clean up old notifications", "err", err)
			}
		case <-tick.C:
		}
	}
}

// Once delivers one batch and returns how many notifications it handled.
func (d *Dispatcher) Once(ctx context.Context) int {
	d.defaults()
	batch, err := d.Store.ClaimNotifications(ctx, d.Batch, d.Lease)
	if err != nil {
		if ctx.Err() == nil {
			d.Log.Warn("could not read the notification queue", "err", err)
		}
		return 0
	}
	for _, o := range batch {
		d.deliver(ctx, o)
	}
	return len(batch)
}

func (d *Dispatcher) deliver(ctx context.Context, o store.Outbound) {
	var err error
	switch {
	case o.Channel == "email" && d.Email != nil:
		err = d.Email.Send(ctx, o.Recipient, Render(o.Event, o.Data, d.PublicURL))
	case o.Channel == "webhook" && d.Webhook != nil:
		var body []byte
		if body, err = Body(o.ID, o.Event, o.CreatedAt, o.Data, d.PublicURL); err == nil {
			err = d.Webhook.Send(ctx, o.ID, body)
		}
	default:
		err = permanent{errChannelOff}
	}
	if err == nil {
		if e := d.Store.MarkSent(ctx, o.ID); e != nil {
			d.Log.Error("delivered but could not record it: it may be sent again", "id", o.ID, "err", e)
		}
		return
	}
	perm := IsPermanent(err)
	retryIn := d.Backoff
	for i := 1; i < o.Attempts && retryIn < d.MaxBackoff; i++ {
		retryIn *= 2
	}
	if retryIn > d.MaxBackoff {
		retryIn = d.MaxBackoff
	}
	giveUp := perm || o.Attempts >= d.MaxAttempts
	// The log names the channel and event, not the recipient or the message.
	d.Log.Warn("notification not delivered", "id", o.ID, "channel", o.Channel, "event", o.Event, "attempt", o.Attempts, "giving_up", giveUp, "err", err)
	if e := d.Store.MarkFailed(ctx, o.ID, err.Error(), perm, d.MaxAttempts, retryIn); e != nil {
		d.Log.Error("could not record a failed delivery", "id", o.ID, "err", e)
	}
}

var errChannelOff = permanentMsg("this channel is no longer configured")

type permanentMsg string

func (p permanentMsg) Error() string { return string(p) }
