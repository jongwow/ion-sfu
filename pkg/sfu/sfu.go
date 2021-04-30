package sfu

import (
	"math/rand"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/pion/ion-sfu/pkg/relay"

	"github.com/go-logr/logr"
	"github.com/pion/ice/v2"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/stats"
	"github.com/pion/turn/v2"
	"github.com/pion/webrtc/v3"
)

// Logger is an implementation of logr.Logger. If is not provided - will be turned off.
var Logger logr.Logger = new(logr.DiscardLogger)

// ICEServerConfig defines parameters for ice servers
type ICEServerConfig struct {
	URLs       []string `mapstructure:"urls"`
	Username   string   `mapstructure:"username"`
	Credential string   `mapstructure:"credential"`
}

type Candidates struct {
	IceLite    bool     `mapstructure:"icelite"`
	NAT1To1IPs []string `mapstructure:"nat1to1"`
}

// WebRTCTransportConfig represents Configuration options
type WebRTCTransportConfig struct {
	Configuration webrtc.Configuration
	Setting       webrtc.SettingEngine
	Router        RouterConfig
	Relay         *relay.Provider
	BufferFactory *buffer.Factory
}

// WebRTCConfig defines parameters for ice
type WebRTCConfig struct {
	ICESinglePort int               `mapstructure:"singleport"`
	ICEPortRange  []uint16          `mapstructure:"portrange"`
	ICEServers    []ICEServerConfig `mapstructure:"iceserver"`
	Candidates    Candidates        `mapstructure:"candidates"`
	SDPSemantics  string            `mapstructure:"sdpsemantics"`
	MDNS          bool              `mapstructure:"mdns"`
}

// Config for base SFU
type Config struct {
	SFU struct {
		Ballast   int64 `mapstructure:"ballast"`
		WithStats bool  `mapstructure:"withstats"`
	} `mapstructure:"sfu"`
	WebRTC        WebRTCConfig `mapstructure:"webrtc"`
	Router        RouterConfig `mapstructure:"Router"`
	Turn          TurnConfig   `mapstructure:"turn"`
	Relay         *relay.Provider
	BufferFactory *buffer.Factory
}

var (
	packetFactory *sync.Pool
)

// SFU represents an sfu instance
type SFU struct {
	sync.RWMutex
	webrtc       WebRTCTransportConfig
	turn         *turn.Server
	sessions     map[string]*session
	datachannels []*Datachannel
	withStats    bool
}

// NewWebRTCTransportConfig parses our settings and returns a usable WebRTCTransportConfig for creating PeerConnections
func NewWebRTCTransportConfig(c Config) WebRTCTransportConfig {
	se := webrtc.SettingEngine{}
	se.DisableMediaEngineCopy(true)

	if c.WebRTC.ICESinglePort != 0 {
		Logger.Info("Listen on ", "single-port", c.WebRTC.ICESinglePort)
		udpListener, err := net.ListenUDP("udp", &net.UDPAddr{
			IP:   net.IP{0, 0, 0, 0},
			Port: c.WebRTC.ICESinglePort,
		})
		if err != nil {
			panic(err)
		}
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpListener))
	} else {
		var icePortStart, icePortEnd uint16

		if c.Turn.Enabled && len(c.Turn.PortRange) == 0 {
			icePortStart = sfuMinPort
			icePortEnd = sfuMaxPort
		} else if len(c.WebRTC.ICEPortRange) == 2 {
			icePortStart = c.WebRTC.ICEPortRange[0]
			icePortEnd = c.WebRTC.ICEPortRange[1]
		}
		if icePortStart != 0 || icePortEnd != 0 {
			if err := se.SetEphemeralUDPPortRange(icePortStart, icePortEnd); err != nil {
				panic(err)
			}
		}
	}

	var iceServers []webrtc.ICEServer
	if c.WebRTC.Candidates.IceLite {
		se.SetLite(c.WebRTC.Candidates.IceLite)
	} else {
		for _, iceServer := range c.WebRTC.ICEServers {
			s := webrtc.ICEServer{
				URLs:       iceServer.URLs,
				Username:   iceServer.Username,
				Credential: iceServer.Credential,
			}
			iceServers = append(iceServers, s)
		}
	}

	se.BufferFactory = c.BufferFactory.GetOrNew

	sdpSemantics := webrtc.SDPSemanticsUnifiedPlan
	switch c.WebRTC.SDPSemantics {
	case "unified-plan-with-fallback":
		sdpSemantics = webrtc.SDPSemanticsUnifiedPlanWithFallback
	case "plan-b":
		sdpSemantics = webrtc.SDPSemanticsPlanB
	}

	w := WebRTCTransportConfig{
		Configuration: webrtc.Configuration{
			ICEServers:   iceServers,
			SDPSemantics: sdpSemantics,
		},
		Setting:       se,
		Router:        c.Router,
		Relay:         c.Relay,
		BufferFactory: c.BufferFactory,
	}

	if len(c.WebRTC.Candidates.NAT1To1IPs) > 0 {
		w.Setting.SetNAT1To1IPs(c.WebRTC.Candidates.NAT1To1IPs, webrtc.ICECandidateTypeHost)
	}

	if !c.WebRTC.MDNS {
		w.Setting.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}

	if c.SFU.WithStats {
		w.Router.WithStats = true
		stats.InitStats()
	}

	return w
}

func init() {
	// Init packet factory
	packetFactory = &sync.Pool{
		New: func() interface{} {
			return make([]byte, 1460)
		},
	}
}

// NewSFU creates a new sfu instance
func NewSFU(c Config) *SFU {

	// Init random seed
	rand.Seed(time.Now().UnixNano())
	// Init ballast
	ballast := make([]byte, c.SFU.Ballast*1024*1024)

	if c.BufferFactory == nil {
		c.BufferFactory = buffer.NewBufferFactory(c.Router.MaxPacketTrack, Logger)
	}

	w := NewWebRTCTransportConfig(c)

	sfu := &SFU{
		webrtc:    w,
		sessions:  make(map[string]*session),
		withStats: c.Router.WithStats,
	}

	if c.Relay != nil {
		c.Relay.SetSettingEngine(w.Setting)
	}

	if c.Turn.Enabled {
		ts, err := InitTurnServer(c.Turn, nil)
		if err != nil {
			Logger.Error(err, "Could not init turn server err")
			os.Exit(1)
		}
		sfu.turn = ts
	}

	runtime.KeepAlive(ballast)
	return sfu
}

// NewSession creates a new session instance
func (s *SFU) newSession(id string) *session {
	session := NewSession(id, s.datachannels, s.webrtc)

	session.OnClose(func() {
		s.Lock()
		delete(s.sessions, id)
		s.Unlock()

		if s.withStats {
			stats.Sessions.Dec()
		}
	})

	s.Lock()
	s.sessions[id] = session
	s.Unlock()

	if s.withStats {
		stats.Sessions.Inc()
	}

	return session
}

// GetSession by id
func (s *SFU) getSession(id string) *session {
	s.RLock()
	defer s.RUnlock()
	return s.sessions[id]
}

func (s *SFU) GetSession(sid string) (*session, WebRTCTransportConfig) {
	session := s.getSession(sid)
	if session == nil {
		session = s.newSession(sid)
	}
	return session, s.webrtc
}

func (s *SFU) NewDatachannel(label string) *Datachannel {
	dc := &Datachannel{Label: label}
	s.datachannels = append(s.datachannels, dc)
	return dc
}

// GetSessions return all sessions
func (s *SFU) GetSessions() map[string]*session {
	s.RLock()
	defer s.RUnlock()
	return s.sessions
}
