// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ipn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"tailscale.com/atomicfile"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netaddr"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/types/persist"
	"tailscale.com/types/preftype"
	"tailscale.com/types/views"
	"tailscale.com/util/dnsname"
)

// DefaultControlURL is the URL base of the control plane
// ("coordination server") for use when no explicit one is configured.
// The default control plane is the hosted version run by Tailscale.com.
const DefaultControlURL = "https://controlplane.tailscale.com"

var (
	// ErrExitNodeIDAlreadySet is returned from (*Prefs).SetExitNodeIP when the
	// Prefs.ExitNodeID field is already set.
	ErrExitNodeIDAlreadySet = errors.New("cannot set ExitNodeIP when ExitNodeID is already set")
)

// IsLoginServerSynonym reports whether a URL is a drop-in replacement
// for the primary Tailscale login server.
func IsLoginServerSynonym(val any) bool {
	return val == "https://login.tailscale.com" || val == "https://controlplane.tailscale.com"
}

// Prefs are the user modifiable settings of the Tailscale node agent.
type Prefs struct {
	// ControlURL is the URL of the control server to use.
	//
	// If empty, the default for new installs, DefaultControlURL
	// is used. It's set non-empty once the daemon has been started
	// for the first time.
	//
	// TODO(apenwarr): Make it safe to update this with SetPrefs().
	// Right now, you have to pass it in the initial prefs in Start(),
	// which is the only code that actually uses the ControlURL value.
	// It would be more consistent to restart controlclient
	// automatically whenever this variable changes.
	//
	// Meanwhile, you have to provide this as part of
	// Options.LegacyMigrationPrefs or Options.UpdatePrefs when
	// calling Backend.Start().
	ControlURL string

	// RouteAll specifies whether to accept subnets advertised by
	// other nodes on the Tailscale network. Note that this does not
	// include default routes (0.0.0.0/0 and ::/0), those are
	// controlled by ExitNodeID/IP below.
	RouteAll bool

	// AllowSingleHosts specifies whether to install routes for each
	// node IP on the tailscale network, in addition to a route for
	// the whole network.
	// This corresponds to the "tailscale up --host-routes" value,
	// which defaults to true.
	//
	// TODO(danderson): why do we have this? It dumps a lot of stuff
	// into the routing table, and a single network route _should_ be
	// all that we need. But when I turn this off in my tailscaled,
	// packets stop flowing. What's up with that?
	AllowSingleHosts bool

	// ExitNodeID and ExitNodeIP specify the node that should be used
	// as an exit node for internet traffic. At most one of these
	// should be non-zero.
	//
	// The preferred way to express the chosen node is ExitNodeID, but
	// in some cases it's not possible to use that ID (e.g. in the
	// linux CLI, before tailscaled has a netmap). For those
	// situations, we allow specifying the exit node by IP, and
	// ipnlocal.LocalBackend will translate the IP into an ID when the
	// node is found in the netmap.
	//
	// If the selected exit node doesn't exist (e.g. it's not part of
	// the current tailnet), or it doesn't offer exit node services, a
	// blackhole route will be installed on the local system to
	// prevent any traffic escaping to the local network.
	ExitNodeID tailcfg.StableNodeID
	ExitNodeIP netip.Addr

	// ExitNodeAllowLANAccess indicates whether locally accessible subnets should be
	// routed directly or via the exit node.
	ExitNodeAllowLANAccess bool

	// CorpDNS specifies whether to install the Tailscale network's
	// DNS configuration, if it exists.
	CorpDNS bool

	// RunSSH bool is whether this node should run an SSH
	// server, permitting access to peers according to the
	// policies as configured by the Tailnet's admin(s).
	RunSSH bool

	// WantRunning indicates whether networking should be active on
	// this node.
	WantRunning bool

	// LoggedOut indicates whether the user intends to be logged out.
	// There are other reasons we may be logged out, including no valid
	// keys.
	// We need to remember this state so that, on next startup, we can
	// generate the "Login" vs "Connect" buttons correctly, without having
	// to contact the server to confirm our nodekey status first.
	LoggedOut bool

	// ShieldsUp indicates whether to block all incoming connections,
	// regardless of the control-provided packet filter. If false, we
	// use the packet filter as provided. If true, we block incoming
	// connections. This overrides tailcfg.Hostinfo's ShieldsUp.
	ShieldsUp bool

	// AdvertiseTags specifies groups that this node wants to join, for
	// purposes of ACL enforcement. These can be referenced from the ACL
	// security policy. Note that advertising a tag doesn't guarantee that
	// the control server will allow you to take on the rights for that
	// tag.
	AdvertiseTags []string

	// Hostname is the hostname to use for identifying the node. If
	// not set, os.Hostname is used.
	Hostname string

	// NotepadURLs is a debugging setting that opens OAuth URLs in
	// notepad.exe on Windows, rather than loading them in a browser.
	//
	// apenwarr 2020-04-29: Unfortunately this is still needed sometimes.
	// Windows' default browser setting is sometimes screwy and this helps
	// users narrow it down a bit.
	NotepadURLs bool

	// ForceDaemon specifies whether a platform that normally
	// operates in "client mode" (that is, requires an active user
	// logged in with the GUI app running) should keep running after the
	// GUI ends and/or the user logs out.
	//
	// The only current applicable platform is Windows. This
	// forced Windows to go into "server mode" where Tailscale is
	// running even with no users logged in. This might also be
	// used for macOS in the future. This setting has no effect
	// for Linux/etc, which always operate in daemon mode.
	ForceDaemon bool `json:"ForceDaemon,omitempty"`

	// Egg is a optional debug flag.
	Egg bool `json:",omitempty"`

	// The following block of options only have an effect on Linux.

	// AdvertiseRoutes specifies CIDR prefixes to advertise into the
	// Tailscale network as reachable through the current
	// node.
	AdvertiseRoutes []netip.Prefix

	// NoSNAT specifies whether to source NAT traffic going to
	// destinations in AdvertiseRoutes. The default is to apply source
	// NAT, which makes the traffic appear to come from the router
	// machine rather than the peer's Tailscale IP.
	//
	// Disabling SNAT requires additional manual configuration in your
	// network to route Tailscale traffic back to the subnet relay
	// machine.
	//
	// Linux-only.
	NoSNAT bool

	// NetfilterMode specifies how much to manage netfilter rules for
	// Tailscale, if at all.
	NetfilterMode preftype.NetfilterMode

	// OperatorUser is the local machine user name who is allowed to
	// operate tailscaled without being root or using sudo.
	OperatorUser string `json:",omitempty"`

	// ProfileName is the desired name of the profile. If empty, then the user's
	// LoginName is used. It is only used for display purposes in the client UI
	// and CLI.
	ProfileName string `json:",omitempty"`

	// AutoUpdate sets the auto-update preferences for the node agent. See
	// AutoUpdatePrefs docs for more details.
	AutoUpdate AutoUpdatePrefs

	// PostureChecking enables the collection of information used for device
	// posture checks.
	PostureChecking bool

	// The Persist field is named 'Config' in the file for backward
	// compatibility with earlier versions.
	// TODO(apenwarr): We should move this out of here, it's not a pref.
	//  We can maybe do that once we're sure which module should persist
	//  it (backend or frontend?)
	Persist *persist.Persist `json:"Config"`
}

// AutoUpdatePrefs are the auto update settings for the node agent.
type AutoUpdatePrefs struct {
	// Check specifies whether background checks for updates are enabled. When
	// enabled, tailscaled will periodically check for available updates and
	// notify the user about them.
	Check bool
	// Apply specifies whether background auto-updates are enabled. When
	// enabled, tailscaled will apply available updates in the background.
	// Check must also be set when Apply is set.
	Apply bool
}

// MaskedPrefs is a Prefs with an associated bitmask of which fields are set.
type MaskedPrefs struct {
	Prefs

	ControlURLSet             bool `json:",omitempty"`
	RouteAllSet               bool `json:",omitempty"`
	AllowSingleHostsSet       bool `json:",omitempty"`
	ExitNodeIDSet             bool `json:",omitempty"`
	ExitNodeIPSet             bool `json:",omitempty"`
	ExitNodeAllowLANAccessSet bool `json:",omitempty"`
	CorpDNSSet                bool `json:",omitempty"`
	RunSSHSet                 bool `json:",omitempty"`
	WantRunningSet            bool `json:",omitempty"`
	LoggedOutSet              bool `json:",omitempty"`
	ShieldsUpSet              bool `json:",omitempty"`
	AdvertiseTagsSet          bool `json:",omitempty"`
	HostnameSet               bool `json:",omitempty"`
	NotepadURLsSet            bool `json:",omitempty"`
	ForceDaemonSet            bool `json:",omitempty"`
	EggSet                    bool `json:",omitempty"`
	AdvertiseRoutesSet        bool `json:",omitempty"`
	NoSNATSet                 bool `json:",omitempty"`
	NetfilterModeSet          bool `json:",omitempty"`
	OperatorUserSet           bool `json:",omitempty"`
	ProfileNameSet            bool `json:",omitempty"`
	AutoUpdateSet             bool `json:",omitempty"`
	PostureCheckingSet        bool `json:",omitempty"`
}

// ApplyEdits mutates p, assigning fields from m.Prefs for each MaskedPrefs
// Set field that's true.
func (p *Prefs) ApplyEdits(m *MaskedPrefs) {
	if p == nil {
		panic("can't edit nil Prefs")
	}
	pv := reflect.ValueOf(p).Elem()
	mv := reflect.ValueOf(m).Elem()
	mpv := reflect.ValueOf(&m.Prefs).Elem()
	fields := mv.NumField()
	for i := 1; i < fields; i++ {
		if mv.Field(i).Bool() {
			newFieldValue := mpv.Field(i - 1)
			pv.Field(i - 1).Set(newFieldValue)
		}
	}
}

// IsEmpty reports whether there are no masks set or if m is nil.
func (m *MaskedPrefs) IsEmpty() bool {
	if m == nil {
		return true
	}
	mv := reflect.ValueOf(m).Elem()
	fields := mv.NumField()
	for i := 1; i < fields; i++ {
		if mv.Field(i).Bool() {
			return false
		}
	}
	return true
}

func (m *MaskedPrefs) Pretty() string {
	if m == nil {
		return "MaskedPrefs{<nil>}"
	}
	var sb strings.Builder
	sb.WriteString("MaskedPrefs{")
	mv := reflect.ValueOf(m).Elem()
	mt := mv.Type()
	mpv := reflect.ValueOf(&m.Prefs).Elem()
	first := true

	format := func(v reflect.Value) string {
		switch v.Type().Kind() {
		case reflect.String:
			return "%s=%q"
		case reflect.Slice:
			// []string
			if v.Type().Elem().Kind() == reflect.String {
				return "%s=%q"
			}
		case reflect.Struct:
			return "%s=%+v"
		case reflect.Pointer:
			if v.Type().Elem().Kind() == reflect.Struct {
				return "%s=%+v"
			}
		}
		return "%s=%v"
	}

	for i := 1; i < mt.NumField(); i++ {
		name := mt.Field(i).Name
		if mv.Field(i).Bool() {
			if !first {
				sb.WriteString(" ")
			}
			first = false
			f := mpv.Field(i - 1)
			fmt.Fprintf(&sb, format(f),
				strings.TrimSuffix(name, "Set"),
				f.Interface())
		}
	}
	sb.WriteString("}")
	return sb.String()
}

// IsEmpty reports whether p is nil or pointing to a Prefs zero value.
func (p *Prefs) IsEmpty() bool { return p == nil || p.Equals(&Prefs{}) }

func (p PrefsView) Pretty() string { return p.ж.Pretty() }

func (p *Prefs) Pretty() string { return p.pretty(runtime.GOOS) }
func (p *Prefs) pretty(goos string) string {
	var sb strings.Builder
	sb.WriteString("Prefs{")
	fmt.Fprintf(&sb, "ra=%v ", p.RouteAll)
	if !p.AllowSingleHosts {
		sb.WriteString("mesh=false ")
	}
	fmt.Fprintf(&sb, "dns=%v want=%v ", p.CorpDNS, p.WantRunning)
	if p.RunSSH {
		sb.WriteString("ssh=true ")
	}
	if p.LoggedOut {
		sb.WriteString("loggedout=true ")
	}
	if p.ForceDaemon {
		sb.WriteString("server=true ")
	}
	if p.NotepadURLs {
		sb.WriteString("notepad=true ")
	}
	if p.ShieldsUp {
		sb.WriteString("shields=true ")
	}
	if p.ExitNodeIP.IsValid() {
		fmt.Fprintf(&sb, "exit=%v lan=%t ", p.ExitNodeIP, p.ExitNodeAllowLANAccess)
	} else if !p.ExitNodeID.IsZero() {
		fmt.Fprintf(&sb, "exit=%v lan=%t ", p.ExitNodeID, p.ExitNodeAllowLANAccess)
	}
	if len(p.AdvertiseRoutes) > 0 || goos == "linux" {
		fmt.Fprintf(&sb, "routes=%v ", p.AdvertiseRoutes)
	}
	if len(p.AdvertiseRoutes) > 0 || p.NoSNAT {
		fmt.Fprintf(&sb, "snat=%v ", !p.NoSNAT)
	}
	if len(p.AdvertiseTags) > 0 {
		fmt.Fprintf(&sb, "tags=%s ", strings.Join(p.AdvertiseTags, ","))
	}
	if goos == "linux" {
		fmt.Fprintf(&sb, "nf=%v ", p.NetfilterMode)
	}
	if p.ControlURL != "" && p.ControlURL != DefaultControlURL {
		fmt.Fprintf(&sb, "url=%q ", p.ControlURL)
	}
	if p.Hostname != "" {
		fmt.Fprintf(&sb, "host=%q ", p.Hostname)
	}
	if p.OperatorUser != "" {
		fmt.Fprintf(&sb, "op=%q ", p.OperatorUser)
	}
	sb.WriteString(p.AutoUpdate.Pretty())
	if p.Persist != nil {
		sb.WriteString(p.Persist.Pretty())
	} else {
		sb.WriteString("Persist=nil")
	}
	sb.WriteString("}")
	return sb.String()
}

func (p PrefsView) ToBytes() []byte {
	return p.ж.ToBytes()
}

func (p *Prefs) ToBytes() []byte {
	data, err := json.MarshalIndent(p, "", "\t")
	if err != nil {
		log.Fatalf("Prefs marshal: %v\n", err)
	}
	return data
}

func (p PrefsView) Equals(p2 PrefsView) bool {
	return p.ж.Equals(p2.ж)
}

func (p *Prefs) Equals(p2 *Prefs) bool {
	if p == nil && p2 == nil {
		return true
	}
	if p == nil || p2 == nil {
		return false
	}

	return p != nil && p2 != nil &&
		p.ControlURL == p2.ControlURL &&
		p.RouteAll == p2.RouteAll &&
		p.AllowSingleHosts == p2.AllowSingleHosts &&
		p.ExitNodeID == p2.ExitNodeID &&
		p.ExitNodeIP == p2.ExitNodeIP &&
		p.ExitNodeAllowLANAccess == p2.ExitNodeAllowLANAccess &&
		p.CorpDNS == p2.CorpDNS &&
		p.RunSSH == p2.RunSSH &&
		p.WantRunning == p2.WantRunning &&
		p.LoggedOut == p2.LoggedOut &&
		p.NotepadURLs == p2.NotepadURLs &&
		p.ShieldsUp == p2.ShieldsUp &&
		p.NoSNAT == p2.NoSNAT &&
		p.NetfilterMode == p2.NetfilterMode &&
		p.OperatorUser == p2.OperatorUser &&
		p.Hostname == p2.Hostname &&
		p.ForceDaemon == p2.ForceDaemon &&
		compareIPNets(p.AdvertiseRoutes, p2.AdvertiseRoutes) &&
		compareStrings(p.AdvertiseTags, p2.AdvertiseTags) &&
		p.Persist.Equals(p2.Persist) &&
		p.ProfileName == p2.ProfileName &&
		p.AutoUpdate == p2.AutoUpdate &&
		p.PostureChecking == p2.PostureChecking
}

func (au AutoUpdatePrefs) Pretty() string {
	if au.Apply {
		return "update=on "
	}
	if au.Check {
		return "update=check "
	}
	return "update=off "
}

func compareIPNets(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func compareStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// NewPrefs returns the default preferences to use.
func NewPrefs() *Prefs {
	// Provide default values for options which might be missing
	// from the json data for any reason. The json can still
	// override them to false.
	return &Prefs{
		// ControlURL is explicitly not set to signal that
		// it's not yet configured, which relaxes the CLI "up"
		// safety net features. It will get set to DefaultControlURL
		// on first up. Or, if not, DefaultControlURL will be used
		// later anyway.
		ControlURL: "",

		RouteAll:         true,
		AllowSingleHosts: true,
		CorpDNS:          true,
		WantRunning:      false,
		NetfilterMode:    preftype.NetfilterOn,
		AutoUpdate: AutoUpdatePrefs{
			Check: true,
			Apply: false,
		},
	}
}

// ControlURLOrDefault returns the coordination server's URL base.
//
// If not configured, or if the configured value is a legacy name equivalent to
// the default, then DefaultControlURL is returned instead.
func (p PrefsView) ControlURLOrDefault() string {
	return p.ж.ControlURLOrDefault()
}

// ControlURLOrDefault returns the coordination server's URL base.
//
// If not configured, or if the configured value is a legacy name equivalent to
// the default, then DefaultControlURL is returned instead.
func (p *Prefs) ControlURLOrDefault() string {
	if p.ControlURL != "" {
		if p.ControlURL != DefaultControlURL && IsLoginServerSynonym(p.ControlURL) {
			return DefaultControlURL
		}
		return p.ControlURL
	}
	return DefaultControlURL
}

// AdminPageURL returns the admin web site URL for the current ControlURL.
func (p PrefsView) AdminPageURL() string { return p.ж.AdminPageURL() }

// AdminPageURL returns the admin web site URL for the current ControlURL.
func (p *Prefs) AdminPageURL() string {
	url := p.ControlURLOrDefault()
	if IsLoginServerSynonym(url) {
		// TODO(crawshaw): In future release, make this https://console.tailscale.com
		url = "https://login.tailscale.com"
	}
	return url + "/admin/machines"
}

// AdvertisesExitNode reports whether p is advertising both the v4 and
// v6 /0 exit node routes.
func (p PrefsView) AdvertisesExitNode() bool { return p.ж.AdvertisesExitNode() }

// AdvertisesExitNode reports whether p is advertising both the v4 and
// v6 /0 exit node routes.
func (p *Prefs) AdvertisesExitNode() bool {
	if p == nil {
		return false
	}
	return tsaddr.ContainsExitRoutes(views.SliceOf(p.AdvertiseRoutes))
}

// SetAdvertiseExitNode mutates p (if non-nil) to add or remove the two
// /0 exit node routes.
func (p *Prefs) SetAdvertiseExitNode(runExit bool) {
	if p == nil {
		return
	}
	all := p.AdvertiseRoutes
	p.AdvertiseRoutes = p.AdvertiseRoutes[:0]
	for _, r := range all {
		if r.Bits() != 0 {
			p.AdvertiseRoutes = append(p.AdvertiseRoutes, r)
		}
	}
	if !runExit {
		return
	}
	p.AdvertiseRoutes = append(p.AdvertiseRoutes,
		netip.PrefixFrom(netaddr.IPv4(0, 0, 0, 0), 0),
		netip.PrefixFrom(netip.IPv6Unspecified(), 0))
}

// peerWithTailscaleIP returns the peer in st with the provided
// Tailscale IP.
func peerWithTailscaleIP(st *ipnstate.Status, ip netip.Addr) (ps *ipnstate.PeerStatus, ok bool) {
	for _, ps := range st.Peer {
		for _, ip2 := range ps.TailscaleIPs {
			if ip == ip2 {
				return ps, true
			}
		}
	}
	return nil, false
}

func isRemoteIP(st *ipnstate.Status, ip netip.Addr) bool {
	for _, selfIP := range st.TailscaleIPs {
		if ip == selfIP {
			return false
		}
	}
	return true
}

// ClearExitNode sets the ExitNodeID and ExitNodeIP to their zero values.
func (p *Prefs) ClearExitNode() {
	p.ExitNodeID = ""
	p.ExitNodeIP = netip.Addr{}
}

// ExitNodeLocalIPError is returned when the requested IP address for an exit
// node belongs to the local machine.
type ExitNodeLocalIPError struct {
	hostOrIP string
}

func (e ExitNodeLocalIPError) Error() string {
	return fmt.Sprintf("cannot use %s as an exit node as it is a local IP address to this machine", e.hostOrIP)
}

func exitNodeIPOfArg(s string, st *ipnstate.Status) (ip netip.Addr, err error) {
	if s == "" {
		return ip, os.ErrInvalid
	}
	ip, err = netip.ParseAddr(s)
	if err == nil {
		// If we're online already and have a netmap, double check that the IP
		// address specified is valid.
		if st.BackendState == "Running" {
			ps, ok := peerWithTailscaleIP(st, ip)
			if !ok {
				return ip, fmt.Errorf("no node found in netmap with IP %v", ip)
			}
			if !ps.ExitNodeOption {
				return ip, fmt.Errorf("node %v is not advertising an exit node", ip)
			}
		}
		if !isRemoteIP(st, ip) {
			return ip, ExitNodeLocalIPError{s}
		}
		return ip, nil
	}
	match := 0
	for _, ps := range st.Peer {
		baseName := dnsname.TrimSuffix(ps.DNSName, st.MagicDNSSuffix)
		if !strings.EqualFold(s, baseName) {
			continue
		}
		match++
		if len(ps.TailscaleIPs) == 0 {
			return ip, fmt.Errorf("node %q has no Tailscale IP?", s)
		}
		if !ps.ExitNodeOption {
			return ip, fmt.Errorf("node %q is not advertising an exit node", s)
		}
		ip = ps.TailscaleIPs[0]
	}
	switch match {
	case 0:
		return ip, fmt.Errorf("invalid value %q for --exit-node; must be IP or unique node name", s)
	case 1:
		if !isRemoteIP(st, ip) {
			return ip, ExitNodeLocalIPError{s}
		}
		return ip, nil
	default:
		return ip, fmt.Errorf("ambiguous exit node name %q", s)
	}
}

// SetExitNodeIP validates and sets the ExitNodeIP from a user-provided string
// specifying either an IP address or a MagicDNS base name ("foo", as opposed to
// "foo.bar.beta.tailscale.net"). This method does not mutate ExitNodeID and
// will fail if ExitNodeID is already set.
func (p *Prefs) SetExitNodeIP(s string, st *ipnstate.Status) error {
	if !p.ExitNodeID.IsZero() {
		return ErrExitNodeIDAlreadySet
	}
	ip, err := exitNodeIPOfArg(s, st)
	if err == nil {
		p.ExitNodeIP = ip
	}
	return err
}

// ShouldSSHBeRunning reports whether the SSH server should be running based on
// the prefs.
func (p PrefsView) ShouldSSHBeRunning() bool {
	return p.Valid() && p.ж.ShouldSSHBeRunning()
}

// ShouldSSHBeRunning reports whether the SSH server should be running based on
// the prefs.
func (p *Prefs) ShouldSSHBeRunning() bool {
	return p.WantRunning && p.RunSSH
}

// PrefsFromBytes deserializes Prefs from a JSON blob.
func PrefsFromBytes(b []byte) (*Prefs, error) {
	p := NewPrefs()
	if len(b) == 0 {
		return p, nil
	}

	if err := json.Unmarshal(b, p); err != nil {
		return nil, err
	}
	return p, nil
}

var jsonEscapedZero = []byte(`\u0000`)

// LoadPrefs loads a legacy relaynode config file into Prefs
// with sensible migration defaults set.
func LoadPrefs(filename string) (*Prefs, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("LoadPrefs open: %w", err) // err includes path
	}
	if bytes.Contains(data, jsonEscapedZero) {
		// Tailscale 1.2.0 - 1.2.8 on Windows had a memory corruption bug
		// in the backend process that ended up sending NULL bytes over JSON
		// to the frontend which wrote them out to JSON files on disk.
		// So if we see one, treat is as corrupt and the user will need
		// to log in again. (better than crashing)
		return nil, os.ErrNotExist
	}
	p, err := PrefsFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("LoadPrefs(%q) decode: %w", filename, err)
	}
	return p, nil
}

func SavePrefs(filename string, p *Prefs) {
	log.Printf("Saving prefs %v %v\n", filename, p.Pretty())
	data := p.ToBytes()
	os.MkdirAll(filepath.Dir(filename), 0700)
	if err := atomicfile.WriteFile(filename, data, 0600); err != nil {
		log.Printf("SavePrefs: %v\n", err)
	}
}

// ProfileID is an auto-generated system-wide unique identifier for a login
// profile. It is a 4 character hex string like "1ab3".
type ProfileID string

// WindowsUserID is a userid (suitable for passing to ipnauth.LookupUserFromID
// or os/user.LookupId) but only set on Windows. It's empty on all other
// platforms, unless envknob.GOOS is in used, making Linux act like Windows for
// tests.
type WindowsUserID string

// LoginProfile represents a single login profile as managed
// by the ProfileManager.
type LoginProfile struct {
	// ID is a unique identifier for this profile.
	// It is assigned on creation and never changes.
	// It may seem redundant to have both ID and UserProfile.ID
	// but they are different things. UserProfile.ID may change
	// over time (e.g. if a device is tagged).
	ID ProfileID

	// Name is the user-visible name of this profile.
	// It is filled in from the UserProfile.LoginName field.
	Name string

	// TailnetMagicDNSName is filled with the MagicDNS suffix for this
	// profile's node (even if MagicDNS isn't necessarily in use).
	// It will neither start nor end with a period.
	//
	// TailnetMagicDNSName is only filled from 2023-09-09 forward,
	// and will only get backfilled when a profile is the current profile.
	TailnetMagicDNSName string

	// Key is the StateKey under which the profile is stored.
	// It is assigned once at profile creation time and never changes.
	Key StateKey

	// UserProfile is the server provided UserProfile for this profile.
	// This is updated whenever the server provides a new UserProfile.
	UserProfile tailcfg.UserProfile

	// NodeID is the NodeID of the node that this profile is logged into.
	// This should be stable across tagging and untagging nodes.
	// It may seem redundant to check against both the UserProfile.UserID
	// and the NodeID. However the NodeID can change if the node is deleted
	// from the admin panel.
	NodeID tailcfg.StableNodeID

	// LocalUserID is the user ID of the user who created this profile.
	// It is only relevant on Windows where we have a multi-user system.
	// It is assigned once at profile creation time and never changes.
	LocalUserID WindowsUserID

	// ControlURL is the URL of the control server that this profile is logged
	// into.
	ControlURL string
}
