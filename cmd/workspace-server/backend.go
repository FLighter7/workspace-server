package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FLighter7/workspace-server/internal/apps"
	"github.com/FLighter7/workspace-server/internal/fsutil"
	"github.com/FLighter7/workspace-server/internal/hub"
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
	var dataFree *int64
	if free, err := fsutil.FreeSpace(s.dataWSS); err == nil {
		dataFree = &free
	}
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
		DataDir:     s.dataWSS,
		DataFree:    dataFree,
		MainServer:  isMainServer(s.serverDir),
		LastError:   s.lastErr,
		Role:        s.link.Role(),
		Term:        s.link.Term(),
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
	link := s.link.Status()
	list := s.apps.List()
	out := make([]web.AppInfo, 0, len(list))
	for _, a := range list {
		info := web.AppInfo{
			App:      a,
			Devices:  counts[a.ID],
			Folder:   s.appFolder(a.ID),
			Diverged: s.divergences(a.ID, link),
			Failing:  link.Failing[a.ID],
			// On a subordinate, an app the main lists: the main sets its
			// quota.
			FollowsMain: s.link.Follows(a.ID),
		}
		// An app whose folder cannot be read shows as empty; the hub
		// reports the error to its devices.
		if st, err := s.store.Stats(a.ID); err == nil {
			info.Size, info.Files = st.Bytes, st.Files
			if !st.Modified.IsZero() {
				modified := st.Modified
				info.Modified = &modified
			}
		}
		out = append(out, info)
	}
	return out
}

// appFolder is the folder of app id's files, under the apps root of the
// data folder in effect.
func (s *server) appFolder(id string) string {
	return filepath.Join(s.dataWSS, storeDir, id)
}

// divergences names the other server of each divergence of app that waits
// for a device's choice, as the hub lists them to the app's devices: on a
// main, its subordinates; on a subordinate, the hub lists this server
// itself, which the page names by the main it diverged from.
func (s *server) divergences(app string, link upstream.Status) []web.Divergence {
	list := s.hub.Divergences(app)
	out := make([]web.Divergence, 0, len(list))
	for _, d := range list {
		div := web.Divergence{ServerID: d.ServerID, Name: d.Name, Since: d.Since}
		if d.ServerID == s.id.ServerID {
			div.ServerID, div.Name = link.ServerID, link.Name
		}
		out = append(out, div)
	}
	return out
}

// SetQuota sets the app's quota, and tells this server's subordinates,
// which take it with the app list. A subordinate refuses for an app it
// follows its main for, whose quota is the main's; an app only it serves
// is its own. The link orders the write with the main's quotas
// (Link.SetLocalQuota), so a link made meanwhile never has it land on an
// app it has just started to follow.
func (s *server) SetQuota(id string, quota int64) error {
	var changed bool
	err := s.link.SetLocalQuota(id, func() error {
		var err error
		changed, err = s.apps.SetQuota(id, quota)
		return err
	})
	if errors.Is(err, upstream.ErrFollowed) {
		return fmt.Errorf("%w: %s", web.ErrFollowsMain, err)
	}
	if err != nil {
		return err
	}
	if changed {
		app, _ := s.apps.Get(id)
		s.logf("app %q: the quota is now %d bytes", id, app.Quota)
		s.hub.AppsChanged()
	}
	return nil
}

// OpenFolder shows the app's folder in the system's file manager. It
// creates nothing: an app that has no folder yet (no files ever written)
// shows the apps' folder that would hold it, or else the WSS-Data folder.
// The app is checked again once the folder is chosen, so an app deleted
// meanwhile opens nothing.
func (s *server) OpenFolder(id string) error {
	if _, ok := s.apps.Get(id); !ok {
		return fmt.Errorf("%w: %q", apps.ErrNotFound, id)
	}
	dir := ""
	for _, d := range []string{s.appFolder(id), filepath.Join(s.dataWSS, storeDir), s.dataWSS} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dir = d
			break
		}
	}
	if dir == "" {
		return fmt.Errorf("the data folder %s is missing", s.dataWSS)
	}
	if _, ok := s.apps.Get(id); !ok {
		return fmt.Errorf("%w: %q", apps.ErrNotFound, id)
	}
	return s.openFolder(dir)
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

func (s *server) Upstream() web.UpstreamStatus {
	return web.UpstreamStatus{Status: s.link.Status(), Subordinates: s.subordinateCount()}
}

// subordinateCount is how many servers are linked to this one: its
// server-kind devices.
func (s *server) subordinateCount() int {
	n := 0
	for _, d := range s.devices.List() {
		if d.IsServer() {
			n++
		}
	}
	return n
}

// LinkUpstream links this server to a main as its subordinate. A main with
// subordinates of its own does not link: they would stop for good, since
// only a main may be an upstream, and so would every device's way to them.
// Should one pair in the meantime, its sessions are closed: it reads the new
// role when it reconnects, and stops.
func (s *server) LinkUpstream(ctx context.Context, code string) error {
	if s.subordinateCount() > 0 {
		return web.ErrHasSubordinates
	}
	if err := s.link.Pair(ctx, code); err != nil {
		return err
	}
	s.hub.CloseServers("")
	return nil
}

// PromoteUpstream makes this server, a subordinate, the main of its family:
// planned (force false) when the main is reachable, which then steps down
// and links here, or forced when it is not.
func (s *server) PromoteUpstream(ctx context.Context, force bool) (bool, error) {
	return s.link.Promote(ctx, force)
}

// handOverWait bounds how long handing the role over waits for the
// subordinate to take it and this server to step down.
const handOverWait = 30 * time.Second

// HandOver asks the subordinate serverID to take the role of main over
// (takeOver), and reports whether this server stepped down to it in time.
// The subordinate syncs the documents first, takes the role, and tells this
// server, which then links to it.
func (s *server) HandOver(ctx context.Context, serverID string) (bool, error) {
	if s.link.Role() != upstream.RoleMain {
		return false, web.ErrNotMain
	}
	switch err := s.hub.TakeOver(serverID); {
	case errors.Is(err, hub.ErrNoSuchServer):
		return false, web.ErrNoSuchServer
	case errors.Is(err, hub.ErrNotConnected):
		return false, web.ErrNotConnected
	case errors.Is(err, hub.ErrDiverged):
		return false, upstream.ErrDiverged
	case err != nil:
		return false, err
	}
	s.logf("asked the subordinate %s to take the role of main over", serverID)
	ctx, cancel := context.WithTimeout(ctx, handOverWait)
	defer cancel()
	for s.link.Role() != upstream.RoleSubordinate {
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	return true, nil
}

// UnlinkUpstream ends the link: this server is a main again, and its data
// stays.
func (s *server) UnlinkUpstream(ctx context.Context) (bool, error) {
	return s.link.Unlink(ctx)
}

// ResumeUpstream resumes an app the write guard stopped, the page's "Resume"
// button: it syncs with the main again at once, with a fresh count.
func (s *server) ResumeUpstream(app string) error {
	return s.link.Resume(app)
}

// InviteUpstream asks the main, over the link, for a pairing code of its own
// for a browser on this computer.
func (s *server) InviteUpstream(ctx context.Context) (string, time.Time, error) {
	inv, err := s.link.Invite(ctx)
	return inv.Code, inv.ExpiresAt, err
}

func (s *server) Devices() []web.DeviceInfo {
	connected := map[string]bool{}
	for _, si := range s.hub.Sessions() {
		connected[si.Device] = true
	}
	list := s.devices.List()
	out := make([]web.DeviceInfo, 0, len(list))
	for _, d := range list {
		info := web.DeviceInfo{
			ID:        d.ID,
			App:       d.App,
			Kind:      d.Kind,
			Name:      d.Name,
			PairedAt:  d.PairedAt,
			LastSeen:  d.LastSeen,
			Connected: connected[d.ID],
		}
		if si := d.Server; si != nil {
			info.Server = &web.LinkedServer{ServerID: si.ServerID, Name: si.Name, Port: si.Port, Hosts: append([]string{}, si.Hosts...)}
		}
		out = append(out, info)
	}
	return out
}

func (s *server) RemoveDevice(id string) bool {
	return s.hub.RemoveDevice(id)
}

// IssuePairing issues a pairing code with a new secret. A subordinate issues
// none: it answers pair with notMain, so its code would only fail. Its page
// asks the main for one instead (InviteUpstream).
func (s *server) IssuePairing() (string, time.Time, error) {
	if s.link.Role() == upstream.RoleSubordinate {
		return "", time.Time{}, web.ErrNotMain
	}
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
		out = append(out, web.AppData{ID: a.ID, Size: a.Size})
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
