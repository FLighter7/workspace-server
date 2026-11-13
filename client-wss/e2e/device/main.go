// Command device is a device for the page's e2e tests, which build it with
// Go as they build the server. It pairs with a main for one app, connects to
// the main's one subordinate on this machine (at 127.0.0.1), and writes the
// app's document data.json there, again and again, each write a transaction,
// which the subordinate's link pushes on to the main: enough of them, quickly
// enough, trip the subordinate's write guard.
//
//	device -code <the main's pairing code> [-app todo] [-pushed 10] [-extra 1]
//
// The first pushed writes are made one at a time, each once the main holds
// the one before (its set has the subordinate's revision); extra more follow
// at once, without waiting. It prints the device id and the last body
// written as JSON, and exits 1 with the reason on stderr when something
// fails.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/wsclient"
)

// budget bounds the whole run: pairing, admission on the subordinate and
// every write.
const budget = 60 * time.Second

func main() {
	code := flag.String("code", "", "the main's pairing code")
	app := flag.String("app", "todo", "the app")
	pushed := flag.Int("pushed", 10, "documents to write, each once the main holds the one before")
	extra := flag.Int("extra", 1, "documents to write after those, at once")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	out, err := run(ctx, *code, *app, *pushed, *extra)
	if err != nil {
		fmt.Fprintln(os.Stderr, "device:", err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

type result struct {
	Device string `json:"device"`
	Last   string `json:"last"` // the last body written
}

// data is the document the device writes: data.json at the app's root.
var data = wsclient.Key{Path: "", ID: "data", Type: "json"}

func run(ctx context.Context, code, app string, pushed, extra int) (result, error) {
	c, err := wsclient.ParseCode(code)
	if err != nil {
		return result{}, err
	}
	up, err := wsclient.Dial(ctx, c.Target, app, wsclient.DialOptions{})
	if err != nil {
		return result{}, fmt.Errorf("reach the main: %w", err)
	}
	defer up.Close()
	p, w, err := up.Pair(ctx, "", c.Secret, "e2e device")
	if err != nil {
		return result{}, fmt.Errorf("pair with the main: %w", err)
	}
	if _, err := up.Head(ctx); err != nil { // confirms the pairing
		return result{}, err
	}
	if len(w.Subordinates) != 1 {
		return result{}, fmt.Errorf("the main lists %d subordinates, want 1", len(w.Subordinates))
	}
	s := w.Subordinates[0]
	sub, etag, err := admitted(ctx, wsclient.Target{Password: s.Password, Fingerprint: s.Fingerprint, Port: s.Port, Hosts: []string{"127.0.0.1"}}, app, p)
	if err != nil {
		return result{}, fmt.Errorf("connect to the subordinate %q: %w", s.Name, err)
	}
	defer sub.Close()

	// Each run writes documents of its own, so that no write repeats an
	// earlier one's bytes.
	run := time.Now().UnixNano()
	var body string
	for i := 1; i <= pushed+extra; i++ {
		body = fmt.Sprintf(`{"run":%d,"n":%d}`, run, i)
		rev, e, err := write(ctx, sub, etag, body)
		if err != nil {
			return result{}, fmt.Errorf("write %d: %w", i, err)
		}
		etag = e
		if i <= pushed {
			if err := reached(ctx, up, rev); err != nil {
				return result{}, fmt.Errorf("write %d did not reach the main: %w", i, err)
			}
		}
	}
	return result{Device: p.Device, Last: body}, nil
}

// write writes body as data.json, which must have ifEtag, in a transaction
// of its own, and returns the set's revision and the document's etag after
// it.
func write(ctx context.Context, cl *wsclient.Client, ifEtag *string, body string) (*string, *string, error) {
	tx := newTx()
	if _, err := cl.Begin(ctx, tx); err != nil {
		return nil, nil, err
	}
	if _, err := cl.Put(ctx, tx, data, ifEtag, int64(len(body)), 0, strings.NewReader(body)); err != nil {
		return nil, nil, err
	}
	r, err := cl.Commit(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if !r.Committed {
		return nil, nil, errors.New("the subordinate's document changed meanwhile")
	}
	for _, d := range r.Docs {
		if d.Key == data {
			return r.Revision, d.Etag, nil
		}
	}
	return nil, nil, errors.New("the commit does not name data.json")
}

func newTx() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "e2e-" + hex.EncodeToString(b)
}

// admitted connects the paired device to the subordinate, which admits it
// once the main's registry has reached it, and returns the session and the
// etag of the subordinate's data.json (nil when there is none).
func admitted(ctx context.Context, t wsclient.Target, app string, p wsclient.Paired) (*wsclient.Client, *string, error) {
	for {
		cl, err := wsclient.Dial(ctx, t, app, wsclient.DialOptions{})
		if err != nil {
			return nil, nil, err
		}
		_, err = cl.Hello(ctx, p.Device, p.Token)
		if err == nil {
			l, err := cl.ListAll(ctx)
			if err != nil {
				cl.Close()
				return nil, nil, err
			}
			for _, d := range l.Docs {
				if d.Key == data {
					etag := d.Etag
					return cl, &etag, nil
				}
			}
			return cl, nil, nil
		}
		cl.Close()
		if !wsclient.HasCode(err, "badToken") {
			return nil, nil, err
		}
		if err := pause(ctx, 200*time.Millisecond); err != nil {
			return nil, nil, errors.New("the subordinate never admitted the device")
		}
	}
}

// reached waits until the main's set has the revision rev.
func reached(ctx context.Context, up *wsclient.Client, rev *string) error {
	for {
		head, err := up.Head(ctx)
		if err != nil {
			return err
		}
		if head != nil && rev != nil && *head == *rev {
			return nil
		}
		if err := pause(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
}

func pause(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
