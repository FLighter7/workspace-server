package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/pairing"
	"github.com/FLighter7/workspace-server/internal/rtc"
	"github.com/FLighter7/workspace-server/internal/upstream"
	"github.com/FLighter7/workspace-server/internal/web"
)

// The web page's backend.

func (s *server) Status() web.Status {
	program, _ := binaryPath()
	s.mu.Lock()
	defer s.mu.Unlock()
	return web.Status{
		Name:        s.cfg.Name,
		ServerID:    s.id.ServerID,
		MDNSName:    s.id.MDNSName,
		MDNSRunning: s.mdnsRunning,
		Listening:   s.listening,
		Addresses:   addrStrings(s.addrs),
		UDPPort:     s.cfg.UDPPort,
		HTTPPort:    s.cfg.HTTPPort,
		Program:     program,
		ServerDir:   s.serverDir,
		DataDir:     s.dataDir,
		MainServer:  isMainServer(s.serverDir),
		LastError:   s.lastErr,
		Role:        s.link.Role(),
	}
}

// SetConfig changes the name and the ports. It checks every value and binds
// every new port before it changes anything, so a refused change leaves the
// server and config.json as they were. A new name shows at once. A new UDP
// port ends the sessions on the old one. A new HTTP port serves the page
// before SetConfig returns; the old one stops once the request that asked
// for the move has been answered.
func (s *server) SetConfig(name *string, udpPort, httpPort *int) error {
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if s.stopping {
		return errors.New("the server is stopping")
	}
	s.mu.Lock()
	old, addrs := s.cfg, s.addrs
	s.mu.Unlock()

	// The flags' ports stay out of config.json: the change goes into both
	// the saved and the effective settings.
	file, eff := s.fileCfg, old
	if name != nil {
		n := strings.TrimSpace(*name)
		file.Name, eff.Name = n, n
	}
	if udpPort != nil {
		file.UDPPort, eff.UDPPort = *udpPort, *udpPort
	}
	if httpPort != nil {
		file.HTTPPort, eff.HTTPPort = *httpPort, *httpPort
	}
	if err := eff.Validate(); err != nil {
		return err
	}
	if err := file.Validate(); err != nil {
		return fmt.Errorf("%s: %w", configFile, err)
	}

	var ln net.Listener
	if eff.HTTPPort != old.HTTPPort {
		var err error
		if ln, err = listenHTTP(eff.HTTPPort); err != nil {
			return err
		}
	}
	var l *rtc.Listener
	if eff.UDPPort != old.UDPPort {
		var err error
		if l, err = s.listen(addrs, eff.UDPPort); err != nil {
			if ln != nil {
				_ = ln.Close()
			}
			return err
		}
	}
	if file != s.fileCfg {
		if err := file.Save(s.cfgPath); err != nil {
			if ln != nil {
				_ = ln.Close()
			}
			if l != nil {
				_ = l.Close()
			}
			return fmt.Errorf("save %s: %w", s.cfgPath, err)
		}
		s.fileCfg = file
	}

	s.mu.Lock()
	s.cfg = eff
	s.mu.Unlock()
	if eff.Name != old.Name {
		s.logf("the name is now %q", eff.Name)
	}
	if l != nil {
		if s.listener != nil {
			if err := s.listener.Close(); err != nil {
				s.logf("close the UDP listener: %v", err)
			}
		}
		s.listener, s.listenErr = l, ""
		s.setListening(true)
		s.logf("devices connect on UDP port %d now; devices paired before must pair again", eff.UDPPort)
	}
	if ln != nil {
		prev := s.page
		s.page = s.serveHTTP(ln)
		if prev != nil {
			// In the background: Shutdown waits for the requests in flight,
			// and the one asking for this move is among them.
			s.httpWG.Go(func() { s.stopHTTP(prev) })
		}
		s.logf("the web page moved to http://localhost:%d/", eff.HTTPPort)
	}
	if eff.Name != old.Name || eff.UDPPort != old.UDPPort {
		// On a subordinate: the main hands the new name and port out to its
		// devices.
		s.link.SelfChanged()
	}
	return nil
}

func (s *server) Apps() []web.AppInfo {
	counts := map[string]int{}
	for _, d := range s.devices.List() {
		counts[d.App]++
	}
	list := s.apps.List()
	out := make([]web.AppInfo, 0, len(list))
	for _, a := range list {
		info := web.AppInfo{App: a, Devices: counts[a.ID]}
		// An app whose folder cannot be read shows as empty; the hub
		// reports the error to its devices.
		if st, err := s.store.Stat(a.ID); err == nil {
			info.Versions = st.Versions
			if st.Exists {
				modified := st.Modified
				info.Size, info.Modified = st.Size, &modified
			}
		}
		out = append(out, info)
	}
	return out
}

func (s *server) AddApp(a apps.App) error {
	if err := s.apps.Add(a); err != nil {
		return err
	}
	s.refusals.Clear(a.ID) // the page's "Add it?" offer is answered
	s.logf("app %q added", a.ID)
	s.hub.AppsChanged()
	return nil
}

func (s *server) DeleteApp(id string) error {
	err := s.hub.RemoveApp(id)
	if !errors.Is(err, apps.ErrNotFound) {
		s.hub.AppsChanged() // the list may have changed even if a later step failed
	}
	if err != nil {
		return err
	}
	s.logf("app %q deleted with its documents and devices", id)
	return nil
}

func (s *server) Upstream() upstream.Status { return s.link.Status() }

// LinkUpstream links this server to a main as its subordinate. The
// subordinates this server had as a main are disconnected: they read its
// new role when they reconnect, and stop.
func (s *server) LinkUpstream(ctx context.Context, code string) error {
	if err := s.link.Pair(ctx, code); err != nil {
		return err
	}
	s.hub.CloseServers()
	return nil
}

func (s *server) Document(id string) ([]byte, bool) {
	body, etag, err := s.store.Read(id)
	return body, err == nil && etag != nil
}

func (s *server) Devices() []web.DeviceInfo {
	connected := map[string]bool{}
	for _, si := range s.hub.Sessions() {
		connected[si.Device] = true
	}
	list := s.devices.List()
	out := make([]web.DeviceInfo, 0, len(list))
	for _, d := range list {
		out = append(out, web.DeviceInfo{
			ID:        d.ID,
			App:       d.App,
			Name:      d.Name,
			PairedAt:  d.PairedAt,
			LastSeen:  d.LastSeen,
			Connected: connected[d.ID],
		})
	}
	return out
}

func (s *server) RemoveDevice(id string) bool {
	return s.hub.RemoveDevice(id)
}

func (s *server) IssuePairing() (string, time.Time, error) {
	secret, expires := s.secrets.Issue()
	s.mu.Lock()
	code := pairing.Code{
		V:  1,
		ID: s.id.ServerID,
		N:  s.cfg.Name,
		P:  s.id.ICEPwd,
		F:  s.fingerprint,
		O:  s.cfg.UDPPort,
		H:  s.hostsLocked(),
		S:  secret,
	}
	s.mu.Unlock()
	return pairing.EncodeCode(code), expires, nil
}

func (s *server) Refusals() []pairing.Refusal {
	return s.refusals.Recent()
}

// The server controls. Each records what was asked and ends the run; the
// first request wins, and a later one while the server stops is ignored.

func (s *server) Stop(removeAutostart bool) {
	s.request(action{kind: actStop, removeAutostart: removeAutostart})
}

func (s *server) Reset() { s.request(action{kind: actReset}) }

func (s *server) DeleteServer() { s.request(action{kind: actDeleteServer}) }

// Restart applies the data-folder change SetDataDir recorded.
func (s *server) Restart() {
	s.mu.Lock()
	move := s.pendingMove
	s.mu.Unlock()
	if move != nil {
		s.request(*move)
	}
}

func (s *server) request(a action) {
	s.mu.Lock()
	first := s.pending == nil
	if first {
		s.pending = &a
	}
	s.mu.Unlock()
	if first && s.cancel != nil {
		s.cancel()
	}
}

func (s *server) CheckDataDir(path string) (web.DataDirCheck, error) {
	c, err := checkDataDir(s.dataDir, path)
	if err != nil {
		return web.DataDirCheck{}, err
	}
	return web.DataDirCheck{
		Path: c.Path, Verdict: string(c.Verdict), Message: c.Message,
		Current: webAppData(c.Current), Target: webAppData(c.Target),
	}, nil
}

func webAppData(list []appData) []web.AppData {
	out := make([]web.AppData, 0, len(list))
	for _, a := range list {
		out = append(out, web.AppData{ID: a.ID, Size: a.Size, Versions: a.Versions})
	}
	return out
}

// SetDataDir checks path again, since the folders may have changed since the
// page asked, refuses a verdict other than the one the page showed, and
// records the move for Restart. The current folder needs no restart. Only
// one change is recorded: a second one, or one while the server stops, is
// refused.
func (s *server) SetDataDir(path, verdict string) (bool, error) {
	c, err := checkDataDir(s.dataDir, path)
	if err != nil {
		return false, err
	}
	switch c.Verdict {
	case verdictSame:
		return false, nil
	case verdictConflict:
		return false, fmt.Errorf("%w: %s", web.ErrDataConflict, c.Path)
	case verdictUnavailable:
		return false, fmt.Errorf("%w: %s", web.ErrDataInvalid, c.Message)
	}
	if string(c.Verdict) != verdict {
		return false, fmt.Errorf("%w: it was %q when checked, it is %q now", web.ErrDataChanged, verdict, c.Verdict)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.pendingMove != nil:
		return false, fmt.Errorf("%w: a change to %s is under way", web.ErrBusy, s.pendingMove.dataDir)
	case s.pending != nil:
		return false, fmt.Errorf("%w: the server is stopping", web.ErrBusy)
	}
	s.pendingMove = &action{kind: actMoveData, dataDir: c.Path, verdict: c.Verdict}
	s.logf("the data folder is to change to %s (%s)", c.Path, c.Verdict)
	return true, nil
}
