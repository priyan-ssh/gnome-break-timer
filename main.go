// Command gnome-break-timer is a zero-bloat, CGO-free break-timer daemon for Linux
// desktop sessions (Fedora / GNOME / Wayland).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// =============================================================================
// Config
// =============================================================================

type TimerConfig struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Enabled     bool   `json:"enabled"`
	IntervalDur string `json:"interval"`
	BreakDur    string `json:"break"`
	OverrideDND bool   `json:"override_dnd"`
	SoundStart  string `json:"sound_start,omitempty"`
	SoundEnd    string `json:"sound_end,omitempty"`
}

func (t TimerConfig) interval() time.Duration {
	d, _ := time.ParseDuration(t.IntervalDur)
	return d
}
func (t TimerConfig) breakDur() time.Duration {
	d, _ := time.ParseDuration(t.BreakDur)
	return d
}

func (t TimerConfig) valid() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("timer has empty name")
	}
	if _, err := time.ParseDuration(t.IntervalDur); err != nil {
		return fmt.Errorf("invalid interval duration: %w", err)
	}
	if _, err := time.ParseDuration(t.BreakDur); err != nil {
		return fmt.Errorf("invalid break duration: %w", err)
	}
	return nil
}

type Config struct {
	Timers         []TimerConfig `json:"timers"`
	SoundEnabled   bool          `json:"sound_enabled"`
	SoundStart     string        `json:"sound_start"`
	SoundEnd       string        `json:"sound_end"`
	NotifyExpireMS int32         `json:"notify_expire_ms"`
}

func (cfg Config) soundsFor(t TimerConfig) (start, end string) {
	start, end = t.SoundStart, t.SoundEnd
	if start == "" {
		start = cfg.SoundStart
	}
	if end == "" {
		end = cfg.SoundEnd
	}
	return start, end
}

func defaultConfig() Config {
	return Config{
		Timers: []TimerConfig{
			{Name: "eyecare", Title: "Eye Care Break", Enabled: true, IntervalDur: "20m", BreakDur: "20s", OverrideDND: false},
			{Name: "movement", Title: "Movement Break", Enabled: true, IntervalDur: "1h", BreakDur: "5m", OverrideDND: false},
		},
		SoundEnabled:   true,
		SoundStart:     "/usr/share/sounds/freedesktop/stereo/message-new-instant.oga",
		SoundEnd:       "/usr/share/sounds/freedesktop/stereo/complete.oga",
		NotifyExpireMS: 15000,
	}
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gnome-break-timer"), nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func loadConfig() (Config, error) {
	cfg := defaultConfig()
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if werr := saveConfig(cfg); werr != nil {
			return cfg, werr
		}
		return cfg, nil
	} else if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}

	valid := cfg.Timers[:0]
	seen := map[string]bool{}
	for _, t := range cfg.Timers {
		if err := t.valid(); err != nil {
			continue
		}
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		valid = append(valid, t)
	}
	cfg.Timers = valid
	if cfg.NotifyExpireMS <= 0 {
		cfg.NotifyExpireMS = 15000
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// =============================================================================
// Socket IPC (CLI <-> daemon)
// =============================================================================

func socketPath() string {
	if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" {
		return filepath.Join(rd, "gnome-break-timer.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("gnome-break-timer-%d.sock", os.Getuid()))
}

func sendCommand(cmd string) (string, error) {
	conn, err := net.DialTimeout("unix", socketPath(), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, cmd); err != nil {
		return "", err
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && reply == "" {
		return "", err
	}
	return strings.TrimRight(reply, "\n"), nil
}

// =============================================================================
// Timer state machine
// =============================================================================

type State string

const (
	StateRunning  State = "running"
	StatePaused   State = "paused"
	StateInBreak  State = "in_break"
	StateDisabled State = "disabled"
)

type timerRuntime struct {
	spec     TimerConfig
	mu       sync.Mutex
	state    State
	nextFire time.Time
	skipCh   chan struct{}
	stopCh   chan struct{}
}

func newTimerRuntime(spec TimerConfig) *timerRuntime {
	return &timerRuntime{
		spec:     spec,
		state:    StateRunning,
		nextFire: time.Now().Add(spec.interval()),
		skipCh:   make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
}

type Daemon struct {
	cfg           Config
	cfgMu         sync.RWMutex
	conn          *dbus.Conn
	timers        map[string]*timerRuntime
	timersMu      sync.RWMutex
	pauseUntil    time.Time
	pauseMu       sync.Mutex
	notifyWaiters map[uint32]chan string
	notifyMu      sync.Mutex
	quit          chan struct{}
}

var indefTime = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

func newDaemon(cfg Config) *Daemon {
	return &Daemon{
		cfg:           cfg,
		timers:        make(map[string]*timerRuntime),
		notifyWaiters: make(map[uint32]chan string),
		quit:          make(chan struct{}),
	}
}

func (d *Daemon) startTimersFromConfig() {
	d.cfgMu.RLock()
	timers := append([]TimerConfig(nil), d.cfg.Timers...)
	d.cfgMu.RUnlock()

	d.timersMu.Lock()
	defer d.timersMu.Unlock()
	for _, spec := range timers {
		if !spec.Enabled {
			continue
		}
		d.startTimerLocked(spec)
	}
}

func (d *Daemon) startTimerLocked(spec TimerConfig) {
	rt := newTimerRuntime(spec)
	d.timers[spec.Name] = rt
	go d.runTimerLoop(rt)
}

func (d *Daemon) stopTimerLocked(name string) {
	if rt, ok := d.timers[name]; ok {
		close(rt.stopCh)
		delete(d.timers, name)
	}
}

func (d *Daemon) applyConfig(newCfg Config) {
	d.cfgMu.Lock()
	d.cfg = newCfg
	d.cfgMu.Unlock()

	d.timersMu.Lock()
	defer d.timersMu.Unlock()

	wanted := make(map[string]TimerConfig, len(newCfg.Timers))
	for _, t := range newCfg.Timers {
		wanted[t.Name] = t
	}

	for name, rt := range d.timers {
		spec, ok := wanted[name]
		if !ok || !spec.Enabled || specChanged(rt.spec, spec) {
			d.stopTimerLocked(name)
		}
	}
	for name, spec := range wanted {
		if !spec.Enabled {
			continue
		}
		if _, running := d.timers[name]; !running {
			d.startTimerLocked(spec)
		}
	}
}

func specChanged(a, b TimerConfig) bool {
	return a.IntervalDur != b.IntervalDur ||
		a.BreakDur != b.BreakDur ||
		a.Title != b.Title ||
		a.SoundStart != b.SoundStart ||
		a.SoundEnd != b.SoundEnd ||
		a.OverrideDND != b.OverrideDND
}

func (d *Daemon) isPaused() (bool, time.Time) {
	d.pauseMu.Lock()
	defer d.pauseMu.Unlock()
	if d.pauseUntil.IsZero() {
		return false, time.Time{}
	}
	if d.pauseUntil != indefTime && time.Now().After(d.pauseUntil) {
		d.pauseUntil = time.Time{}
		return false, time.Time{}
	}
	return true, d.pauseUntil
}

func (d *Daemon) setPause(dur time.Duration) {
	d.pauseMu.Lock()
	defer d.pauseMu.Unlock()
	if dur <= 0 {
		d.pauseUntil = indefTime
	} else {
		d.pauseUntil = time.Now().Add(dur)
	}
}

func (d *Daemon) clearPause() {
	d.pauseMu.Lock()
	defer d.pauseMu.Unlock()
	d.pauseUntil = time.Time{}
}

// =============================================================================
// DBus notifications
// =============================================================================

const notifyDest = "org.freedesktop.Notifications"
const notifyPath = "/org/freedesktop/Notifications"

func connectDBus() (*dbus.Conn, error) {
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("connecting to session bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("dbus auth: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("dbus hello: %w", err)
	}
	return conn, nil
}

func (d *Daemon) listenActions() error {
	call := d.conn.BusObject().Call(
		"org.freedesktop.DBus.AddMatch", 0,
		"type='signal',interface='org.freedesktop.Notifications',member='ActionInvoked'")
	if call.Err != nil {
		return call.Err
	}
	call = d.conn.BusObject().Call(
		"org.freedesktop.DBus.AddMatch", 0,
		"type='signal',interface='org.freedesktop.Notifications',member='NotificationClosed'")
	if call.Err != nil {
		return call.Err
	}

	ch := make(chan *dbus.Signal, 16)
	d.conn.Signal(ch)

	go func() {
		for {
			select {
			case sig, ok := <-ch:
				if !ok {
					return
				}
				d.handleSignal(sig)
			case <-d.quit:
				return
			}
		}
	}()
	return nil
}

func (d *Daemon) handleSignal(sig *dbus.Signal) {
	if len(sig.Body) < 2 {
		return
	}
	id, ok := sig.Body[0].(uint32)
	if !ok {
		return
	}
	switch sig.Name {
	case "org.freedesktop.Notifications.ActionInvoked":
		action, ok := sig.Body[1].(string)
		if !ok {
			return
		}
		d.notifyMu.Lock()
		waiter, exists := d.notifyWaiters[id]
		d.notifyMu.Unlock()
		if exists {
			select {
			case waiter <- action:
			default:
			}
		}
	case "org.freedesktop.Notifications.NotificationClosed":
		d.notifyMu.Lock()
		waiter, exists := d.notifyWaiters[id]
		d.notifyMu.Unlock()
		if exists {
			select {
			case waiter <- "":
			default:
			}
		}
	}
}

func (d *Daemon) notify(summary, body string, actions []string, expireMS int32, override bool) (uint32, chan string, error) {
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(1))}
	if override {
		hints["urgency"] = dbus.MakeVariant(byte(2)) // 2 = CRITICAL, punches through DND
	}

	obj := d.conn.Object(notifyDest, dbus.ObjectPath(notifyPath))
	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"BreakTimer", uint32(0), "preferences-system-time", summary, body, actions, hints, expireMS)
	if call.Err != nil {
		return 0, nil, call.Err
	}
	var id uint32
	if err := call.Store(&id); err != nil {
		return 0, nil, err
	}
	waiter := make(chan string, 1)
	d.notifyMu.Lock()
	d.notifyWaiters[id] = waiter
	d.notifyMu.Unlock()
	return id, waiter, nil
}

func (d *Daemon) closeNotifyWaiter(id uint32) {
	d.notifyMu.Lock()
	delete(d.notifyWaiters, id)
	d.notifyMu.Unlock()
}

func (d *Daemon) simpleNotify(summary, body string) {
	obj := d.conn.Object(notifyDest, dbus.ObjectPath(notifyPath))
	obj.Call("org.freedesktop.Notifications.Notify", 0,
		"BreakTimer", uint32(0), "preferences-system-time", summary, body,
		[]string{}, map[string]dbus.Variant{}, int32(5000))
}

func playSound(cfg Config, path string) {
	if !cfg.SoundEnabled || path == "" {
		return
	}
	if _, err := exec.LookPath("paplay"); err != nil {
		return
	}
	go func() {
		_ = exec.Command("paplay", path).Run()
	}()
}

// =============================================================================
// Break flow / per-timer loop (Ultra-efficient 0-idle loop)
// =============================================================================

func (d *Daemon) runTimerLoop(t *timerRuntime) {
	// Native ticker, no 1-second busy loops
	ticker := time.NewTicker(t.spec.interval())
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-d.quit:
			return
		case <-ticker.C:
			paused, _ := d.isPaused()
			t.mu.Lock()
			if paused {
				t.state = StatePaused
				t.nextFire = time.Now().Add(t.spec.interval())
				t.mu.Unlock()
				continue
			}
			t.mu.Unlock()
			
			// Fire the break!
			d.runBreakFlow(t)
			
			// Reset clock post-break
			t.mu.Lock()
			t.state = StateRunning
			t.nextFire = time.Now().Add(t.spec.interval())
			t.mu.Unlock()
		
		case <-t.skipCh:
			// Shift the clock forward and restart the ticker cleanly
			t.mu.Lock()
			t.nextFire = time.Now().Add(t.spec.interval())
			t.mu.Unlock()
			ticker.Reset(t.spec.interval())
		}
	}
}

func (d *Daemon) runBreakFlow(t *timerRuntime) {
	t.mu.Lock()
	t.state = StateInBreak
	t.mu.Unlock()

	d.cfgMu.RLock()
	cfg := d.cfg
	d.cfgMu.RUnlock()
	soundStart, soundEnd := cfg.soundsFor(t.spec)

	playSound(cfg, soundStart)

	actions := []string{"accept", "Accept Break", "reject", "Skip", "pause", "Snooze 30m"}
	body := fmt.Sprintf("Time for a %s. Click Accept to start now, or it will start automatically.", humanDuration(t.spec.breakDur()))
	
	id, waiter, err := d.notify(t.spec.Title, body, actions, cfg.NotifyExpireMS, t.spec.OverrideDND)
	if err != nil {
		return
	}
	defer d.closeNotifyWaiter(id)

	var action string
	select {
	case action = <-waiter:
	case <-time.After(time.Duration(cfg.NotifyExpireMS) * time.Millisecond):
		action = ""
	case <-d.quit:
		return
	case <-t.stopCh:
		return
	}

	switch action {
	case "reject":
		return
	case "pause":
		d.setPause(30 * time.Minute)
		d.simpleNotify("Break Timer Snoozed", "All timers snoozed for 30 minutes.")
		return
	case "accept", "":
	}

	d.simpleNotify(t.spec.Title, fmt.Sprintf("Break started — %s.", humanDuration(t.spec.breakDur())))
	select {
	case <-time.After(t.spec.breakDur()):
	case <-t.skipCh:
	case <-d.quit:
		return
	case <-t.stopCh:
		return
	}
	playSound(cfg, soundEnd)
	d.simpleNotify(t.spec.Title, "Break over — back to work!")
}

func humanDuration(dur time.Duration) string {
	if dur < time.Minute {
		return fmt.Sprintf("%d seconds", int(dur.Seconds()))
	}
	return fmt.Sprintf("%d minutes", int(dur.Minutes()))
}

// =============================================================================
// Control socket server & commands
// =============================================================================

func (d *Daemon) serveControlSocket() error {
	path := socketPath()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	go func() {
		<-d.quit
		ln.Close()
		_ = os.Remove(path)
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handleControlConn(conn)
		}
	}()
	return nil
}

func (d *Daemon) handleControlConn(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	reply := d.dispatchControl(strings.TrimSpace(line))
	fmt.Fprintln(conn, reply)
}

func (d *Daemon) dispatchControl(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "ERR empty command"
	}
	switch fields[0] {
	case "status":
		return d.statusJSON()
	case "pause":
		dur, err := parsePauseArg(fields)
		if err != nil {
			return "ERR invalid duration: " + err.Error()
		}
		d.setPause(dur)
		return "OK snoozed"
	case "resume":
		d.clearPause()
		return "OK resumed"
	case "skip":
		if len(fields) < 2 {
			return "ERR usage: skip <timer-name>"
		}
		d.timersMu.RLock()
		t, ok := d.timers[fields[1]]
		d.timersMu.RUnlock()
		if !ok {
			return "ERR unknown timer"
		}
		select {
		case t.skipCh <- struct{}{}:
		default:
		}
		return "OK skipped"
	case "enable", "disable":
		if len(fields) < 2 {
			return "ERR usage: " + fields[0] + " <timer-name>"
		}
		return d.setTimerEnabled(fields[1], fields[0] == "enable")
	case "set":
		if len(fields) < 6 {
			return "ERR usage: set <id> <interval> <break> <override_dnd:true/false> <Title...>"
		}
		id := fields[1]
		interval := fields[2]
		breakDur := fields[3]
		override := fields[4] == "true"
		title := strings.Join(fields[5:], " ")
		return d.setTimerConfig(id, interval, breakDur, override, title)
	case "delete":
		if len(fields) < 2 {
			return "ERR usage: delete <id>"
		}
		return d.deleteTimerConfig(fields[1])
	case "reload":
		cfg, err := loadConfig()
		if err != nil {
			return "ERR " + err.Error()
		}
		d.applyConfig(cfg)
		return "OK reloaded"
	case "ping":
		return "OK pong"
	default:
		return "ERR unknown command " + fields[0]
	}
}

func (d *Daemon) setTimerConfig(id, interval, breakDur string, override bool, title string) string {
	d.cfgMu.Lock()
	cfg := d.cfg
	d.cfgMu.Unlock()

	if _, err := time.ParseDuration(interval); err != nil {
		return "ERR invalid interval format"
	}
	if _, err := time.ParseDuration(breakDur); err != nil {
		return "ERR invalid break format"
	}

	found := false
	for i, t := range cfg.Timers {
		if t.Name == id {
			cfg.Timers[i].IntervalDur = interval
			cfg.Timers[i].BreakDur = breakDur
			cfg.Timers[i].OverrideDND = override
			cfg.Timers[i].Title = title
			found = true
			break
		}
	}
	if !found {
		cfg.Timers = append(cfg.Timers, TimerConfig{
			Name: id, Title: title, IntervalDur: interval, BreakDur: breakDur, OverrideDND: override, Enabled: true,
		})
	}
	if err := saveConfig(cfg); err != nil {
		return "ERR saving config: " + err.Error()
	}
	d.applyConfig(cfg)
	return "OK saved " + id
}

func (d *Daemon) deleteTimerConfig(id string) string {
	d.cfgMu.Lock()
	cfg := d.cfg
	d.cfgMu.Unlock()

	var newTimers []TimerConfig
	found := false
	for _, t := range cfg.Timers {
		if t.Name == id {
			found = true
			continue
		}
		newTimers = append(newTimers, t)
	}
	if !found {
		return "ERR unknown timer " + id
	}
	cfg.Timers = newTimers
	if err := saveConfig(cfg); err != nil {
		return "ERR saving config: " + err.Error()
	}
	d.applyConfig(cfg)
	return "OK deleted " + id
}

func (d *Daemon) setTimerEnabled(name string, enabled bool) string {
	d.cfgMu.RLock()
	cfg := d.cfg
	d.cfgMu.RUnlock()

	found := false
	for i := range cfg.Timers {
		if cfg.Timers[i].Name == name {
			cfg.Timers[i].Enabled = enabled
			found = true
			break
		}
	}
	if !found {
		return "ERR unknown timer " + name
	}
	if err := saveConfig(cfg); err != nil {
		return "ERR saving config: " + err.Error()
	}
	d.applyConfig(cfg)
	return "OK toggled " + name
}

func parsePauseArg(fields []string) (time.Duration, error) {
	if len(fields) < 2 || fields[1] == "indef" || fields[1] == "indefinite" {
		return 0, nil
	}
	return time.ParseDuration(fields[1])
}

type timerStatus struct {
	Name         string `json:"name"`
	Title        string `json:"title"`
	State        State  `json:"state"`
	NextFireIn   string `json:"next_fire_in,omitempty"`
	NextFireAt   string `json:"next_fire_at,omitempty"`
	IntervalDur  string `json:"interval"`
	BreakDur     string `json:"break"`
	OverrideDND  bool   `json:"override_dnd"`
}

type daemonStatus struct {
	Paused     bool          `json:"paused"`
	PauseUntil string        `json:"pause_until,omitempty"`
	PauseIndef bool          `json:"pause_indefinite,omitempty"`
	Timers     []timerStatus `json:"timers"`
}

func (d *Daemon) statusJSON() string {
	d.cfgMu.RLock()
	cfg := d.cfg
	d.cfgMu.RUnlock()
	paused, until := d.isPaused()
	ds := daemonStatus{Paused: paused}
	if paused {
		if until == indefTime {
			ds.PauseIndef = true
		} else {
			ds.PauseUntil = until.Format(time.RFC3339)
		}
	}
	d.timersMu.RLock()
	for _, spec := range cfg.Timers {
		if rt, ok := d.timers[spec.Name]; ok {
			ds.Timers = append(ds.Timers, timerSnapshot(rt))
		} else {
			ds.Timers = append(ds.Timers, timerStatus{
				Name: spec.Name, Title: spec.Title, State: StateDisabled,
				IntervalDur: spec.IntervalDur, BreakDur: spec.BreakDur, OverrideDND: spec.OverrideDND,
			})
		}
	}
	d.timersMu.RUnlock()
	data, _ := json.Marshal(ds)
	return string(data)
}

func timerSnapshot(t *timerRuntime) timerStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return timerStatus{
		Name: t.spec.Name, Title: t.spec.Title, State: t.state,
		NextFireIn: time.Until(t.nextFire).Round(time.Second).String(), NextFireAt: t.nextFire.Format(time.RFC3339),
		IntervalDur: t.spec.IntervalDur, BreakDur: t.spec.BreakDur, OverrideDND: t.spec.OverrideDND,
	}
}

// =============================================================================
// Daemon entrypoint
// =============================================================================

func runDaemon() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	conn, err := connectDBus()
	if err != nil {
		return err
	}
	defer conn.Close()

	d := newDaemon(cfg)
	d.conn = conn

	if err := d.listenActions(); err != nil {
		return err
	}
	if err := d.serveControlSocket(); err != nil {
		return err
	}
	d.startTimersFromConfig()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	close(d.quit)
	time.Sleep(200 * time.Millisecond)
	return nil
}

// =============================================================================
// Zenity GUI integration (Clean SNOOZED status mapping)
// =============================================================================

func zenityAvailable() bool {
	_, err := exec.LookPath("zenity")
	return err == nil
}

func getLiveStatusSummary() string {
	reply, err := sendCommand("status")
	if err != nil {
		return "DAEMON STATUS: Not Running (Start daemon to activate)"
	}
	var ds daemonStatus
	if err := json.Unmarshal([]byte(reply), &ds); err != nil {
		return "DAEMON STATUS: Running"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[ Dashboard Snapshot at %s ]\n(Click 'Refresh Dashboard' below for live countdowns)\n\n", time.Now().Format("15:04:05")))
	sb.WriteString("ACTIVE TIMERS:\n")
	
	for _, t := range ds.Timers {
		dndStr := ""
		if t.OverrideDND {
			dndStr = " [Fullscreen Override]"
		}

		// Simplified UI masking for paused/snoozed state
		if ds.Paused || t.State == StatePaused {
			sb.WriteString(fmt.Sprintf("  • %s: SNOOZED%s\n", t.Title, dndStr))
		} else if t.State == StateDisabled {
			sb.WriteString(fmt.Sprintf("  • %s: OFF%s\n", t.Title, dndStr))
		} else if t.State == StateInBreak {
			sb.WriteString(fmt.Sprintf("  • %s: IN BREAK NOW!%s\n", t.Title, dndStr))
		} else {
			sb.WriteString(fmt.Sprintf("  • %s: Next break in %s%s\n", t.Title, t.NextFireIn, dndStr))
		}
	}
	sb.WriteString("__________________________________")
	return sb.String()
}

func runZenityMenu() error {
	if !zenityAvailable() {
		return fmt.Errorf("zenity not found on PATH; install it with 'sudo dnf install zenity'")
	}

	// Auto-start daemon if not running
	if _, err := sendCommand("ping"); err != nil {
		cmd := exec.Command(os.Args[0], "daemon")
		if err := cmd.Start(); err == nil {
			time.Sleep(500 * time.Millisecond) // Give it a moment to bind socket
		}
	}

	for {
		headerStatus := getLiveStatusSummary()

		out, err := exec.Command("zenity", "--list",
			"--title=GNOME Break Timer",
			"--text="+headerStatus,
			"--column=Option",
			"Refresh Dashboard",
			"Snooze/Pause All Timers",
			"Resume All Timers",
			"Turn Timers On/Off",
			"Create New Timer",
			"Edit Existing Timer",
			"Delete Timer",
			"--height=560", "--width=480",
		).Output()

		choice := strings.TrimSpace(string(out))
		if err != nil || choice == "" {
			return nil 
		}

		switch choice {
		case "Refresh Dashboard":
			// Loops back and re-renders live state
		case "Snooze/Pause All Timers":
			zenityPauseAll()
		case "Resume All Timers":
			zenityRunControl("resume", "Timers resumed.")
		case "Turn Timers On/Off":
			zenityManageTimers()
		case "Create New Timer":
			zenityCreateTimer()
		case "Edit Existing Timer":
			zenityEditTimer()
		case "Delete Timer":
			zenityDeleteTimer()
		}
	}
}

func zenityPauseAll() error {
	out, err := exec.Command("zenity", "--entry",
		"--title=Snooze/Pause All Timers",
		"--text=Enter duration (e.g. 30m, 1h, indef):",
		"--entry-text=30m").Output()
	if err != nil {
		return nil
	}
	input := strings.TrimSpace(string(out))
	if input == "" {
		return nil
	}
	if input != "indef" && input != "indefinite" {
		if _, err := time.ParseDuration(input); err != nil {
			zenityError("Invalid duration format. Use values like '30m', '1h', '90s', or 'indef'.")
			return err
		}
	}
	cmd := "pause " + input
	var okMsg string
	if input == "indef" || input == "indefinite" {
		okMsg = "All timers paused indefinitely."
	} else {
		okMsg = fmt.Sprintf("All timers paused for %s.", input)
	}
	return zenityRunControl(cmd, okMsg)
}

func zenityRunControl(cmd, okMsg string) error {
	reply, err := sendCommand(cmd)
	if err != nil {
		zenityError("Daemon isn't running. Start it with 'gnome-break-timer daemon'.")
		return err
	}
	if strings.HasPrefix(reply, "ERR") {
		zenityError(reply)
		return fmt.Errorf(reply)
	}
	exec.Command("zenity", "--info", "--text="+okMsg, "--timeout=2").Run()
	return nil
}

func zenityError(msg string) {
	exec.Command("zenity", "--error", "--text="+msg).Run()
}

func zenityManageTimers() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	args := []string{"--list", "--checklist", "--title=Turn Timers On/Off", "--column=Active", "--column=ID", "--column=Schedule"}
	for _, t := range cfg.Timers {
		checked := "FALSE"
		if t.Enabled {
			checked = "TRUE"
		}
		dndStr := ""
		if t.OverrideDND {
			dndStr = " [Fullscreen Override]"
		}
		args = append(args, checked, t.Name, fmt.Sprintf("%s (%s/%s)%s", t.Title, t.IntervalDur, t.BreakDur, dndStr))
	}
	out, err := exec.Command("zenity", args...).Output()
	if err != nil {
		return nil
	}
	checkedNames := map[string]bool{}
	for _, n := range strings.Split(strings.TrimSpace(string(out)), "|") {
		if n != "" {
			checkedNames[n] = true
		}
	}
	for i := range cfg.Timers {
		cfg.Timers[i].Enabled = checkedNames[cfg.Timers[i].Name]
	}
	saveConfig(cfg)
	sendCommand("reload")
	return nil
}

func zenityCreateTimer() error {
	out, err := exec.Command("zenity", "--forms", "--title=Create New Timer",
		"--text=Use duration suffixes like '45m', '90s', or '1h'.",
		"--add-entry=Title (e.g. Drink Water)",
		"--add-entry=Interval (e.g. 45m)",
		"--add-entry=Break (e.g. 15s)",
		"--add-entry=Show over full-screen? (yes/no)",
		"--separator=|").Output()
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) < 4 || parts[0] == "" {
		return nil
	}
	
	title := parts[0]
	interval := parts[1]
	breakDur := parts[2]
	ov := strings.ToLower(strings.TrimSpace(parts[3]))
	override := "false"
	if ov == "yes" || ov == "y" || ov == "true" {
		override = "true"
	}
	
	id := strings.ToLower(strings.ReplaceAll(title, " ", "_"))
	cmd := fmt.Sprintf("set %s %s %s %s %s", id, interval, breakDur, override, title)
	return zenityRunControl(cmd, "Timer created successfully.")
}

func zenityEditTimer() error {
	cfg, err := loadConfig()
	if err != nil || len(cfg.Timers) == 0 {
		zenityError("No timers to edit.")
		return err
	}
	
	args := []string{"--list", "--title=Select Timer to Edit", "--text=Which timer do you want to update?", "--column=ID", "--column=Title"}
	for _, t := range cfg.Timers {
		args = append(args, t.Name, t.Title)
	}
	outID, err := exec.Command("zenity", args...).Output()
	id := strings.TrimSpace(string(outID))
	if err != nil || id == "" {
		return nil
	}
	
	var old TimerConfig
	for _, t := range cfg.Timers {
		if t.Name == id {
			old = t
			break
		}
	}

	formText := fmt.Sprintf("Editing: %s\n\nCurrent Values:\nInterval: %s | Break: %s | Full-screen: %v\n\n(Leave a field BLANK to keep the current value)", old.Title, old.IntervalDur, old.BreakDur, old.OverrideDND)
	
	outForm, err := exec.Command("zenity", "--forms", "--title=Edit Timer",
		"--text="+formText,
		"--add-entry=New Title",
		"--add-entry=New Interval (e.g. 45m)",
		"--add-entry=New Break (e.g. 15s)",
		"--add-entry=Show over full-screen? (yes/no)",
		"--separator=|").Output()
	if err != nil {
		return nil
	}
	
	parts := strings.Split(strings.TrimSpace(string(outForm)), "|")
	if len(parts) < 4 {
		return nil
	}
	
	title := parts[0]; if title == "" { title = old.Title }
	interval := parts[1]; if interval == "" { interval = old.IntervalDur }
	breakDur := parts[2]; if breakDur == "" { breakDur = old.BreakDur }
	
	var override string
	if parts[3] == "" {
		override = fmt.Sprintf("%v", old.OverrideDND)
	} else {
		ov := strings.ToLower(strings.TrimSpace(parts[3]))
		if ov == "yes" || ov == "y" || ov == "true" {
			override = "true"
		} else {
			override = "false"
		}
	}
	
	cmd := fmt.Sprintf("set %s %s %s %s %s", old.Name, interval, breakDur, override, title)
	return zenityRunControl(cmd, "Timer updated successfully.")
}

func zenityDeleteTimer() error {
	cfg, err := loadConfig()
	if err != nil || len(cfg.Timers) == 0 {
		zenityError("No timers to delete.")
		return err
	}
	args := []string{"--list", "--title=Delete Timer", "--text=Select a timer to delete permanently", "--column=ID", "--column=Title"}
	for _, t := range cfg.Timers {
		args = append(args, t.Name, t.Title)
	}
	out, err := exec.Command("zenity", args...).Output()
	id := strings.TrimSpace(string(out))
	if err != nil || id == "" {
		return nil
	}
	return zenityRunControl("delete "+id, "Timer deleted.")
}

// =============================================================================
// CLI
// =============================================================================

func main() {
	if len(os.Args) < 2 {
		os.Exit(1)
	}
	switch os.Args[1] {
	case "daemon":
		runDaemon()
	case "menu":
		runZenityMenu()
	case "pause":
		arg := "indef"
		if len(os.Args) > 2 {
			arg = os.Args[2]
		}
		reply, _ := sendCommand("pause " + arg)
		fmt.Println(reply)
	case "resume":
		reply, _ := sendCommand("resume")
		fmt.Println(reply)
	case "set":
		if len(os.Args) < 7 {
			fmt.Println("usage: gnome-break-timer set <id> <interval> <break> <override_dnd:true/false> <Title...>")
			os.Exit(1)
		}
		cmd := fmt.Sprintf("set %s %s %s %s %s", os.Args[2], os.Args[3], os.Args[4], os.Args[5], strings.Join(os.Args[6:], " "))
		reply, _ := sendCommand(cmd)
		fmt.Println(reply)
	case "delete":
		if len(os.Args) < 3 {
			os.Exit(1)
		}
		reply, _ := sendCommand("delete " + os.Args[2])
		fmt.Println(reply)
	case "status":
		reply, _ := sendCommand("status")
		fmt.Println(reply)
	}
}