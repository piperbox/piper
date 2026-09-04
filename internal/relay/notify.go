package relay

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// listen holds one dedicated connection LISTENing on channels for the life
// of ctx and calls handle for every notification. The store's database/sql
// pool cannot LISTEN (a notification arrives on one specific connection), so
// this dials pgx directly on the same DSN. On every connect — first and
// after each drop — it calls resync before handling anything, so a NOTIFY
// missed while disconnected costs one full reload, never correctness.
// Reconnects back off from 1 s to 15 s. Channel names are package constants,
// never user input.
func listen(ctx context.Context, dsn string, channels []string, resync func(), handle func(channel, payload string)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := listenOnce(ctx, dsn, channels, resync, handle)
		if ctx.Err() != nil {
			return
		}
		log.Printf("relay: notify listener lost (%v); reconnecting in %s", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

func listenOnce(ctx context.Context, dsn string, channels []string, resync func(), handle func(channel, payload string)) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return err
		}
	}
	resync()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		handle(n.Channel, n.Payload)
	}
}
