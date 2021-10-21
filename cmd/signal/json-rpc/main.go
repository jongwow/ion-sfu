// Package cmd contains an entrypoint for running an ion-sfu instance.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/gorilla/websocket"
	"github.com/pion/ion-sfu/cmd/signal/json-rpc/server"
	log "github.com/pion/ion-sfu/pkg/logger"
	"github.com/pion/ion-sfu/pkg/middlewares/datachannel"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sourcegraph/jsonrpc2"
	websocketjsonrpc2 "github.com/sourcegraph/jsonrpc2/websocket"
	"github.com/spf13/viper"
)

// logC need to get logger options from config
type logC struct {
	Config log.GlobalConfig `mapstructure:"log"`
}

var (
	conf           = sfu.Config{}
	file           string
	cert           string
	key            string
	addr           string
	metricsAddr    string
	verbosityLevel int
	logConfig      logC
	logger         = log.New()
)

const (
	portRangeLimit = 100
)

func showHelp() {
	fmt.Printf("Usage:%s {params}\n", os.Args[0])
	fmt.Println("      -c {config file}")
	fmt.Println("      -cert {cert file}")
	fmt.Println("      -key {key file}")
	fmt.Println("      -a {listen addr}")
	fmt.Println("      -h (show help info)")
	fmt.Println("      -v {0-10} (verbosity level, default 0)")
}

func load() bool {
	_, err := os.Stat(file)
	if err != nil {
		return false
	}

	viper.SetConfigFile(file)
	viper.SetConfigType("toml")

	err = viper.ReadInConfig()
	if err != nil {
		logger.Error(err, "config file read failed", "file", file)
		return false
	}
	err = viper.GetViper().Unmarshal(&conf)
	if err != nil {
		logger.Error(err, "sfu config file loaded failed", "file", file)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) > 2 {
		logger.Error(nil, "config file loaded failed. webrtc port must be [min,max]", "file", file)
		return false
	}

	if len(conf.WebRTC.ICEPortRange) != 0 && conf.WebRTC.ICEPortRange[1]-conf.WebRTC.ICEPortRange[0] < portRangeLimit {
		logger.Error(nil, "config file loaded failed. webrtc port must be [min, max] and max - min >= portRangeLimit", "file", file, "portRangeLimit", portRangeLimit)
		return false
	}

	if len(conf.Turn.PortRange) > 2 {
		logger.Error(nil, "config file loaded failed. turn port must be [min,max]", "file", file)
		return false
	}

	if logConfig.Config.V < 0 {
		logger.Error(nil, "Logger V-Level cannot be less than 0")
		return false
	}

	logger.V(0).Info("Config file loaded", "file", file)
	return true
}

func parse() bool {
	flag.StringVar(&file, "c", "config.toml", "config file")
	flag.StringVar(&cert, "cert", "", "cert file")
	flag.StringVar(&key, "key", "", "key file")
	flag.StringVar(&addr, "a", ":7000", "address to use")
	flag.StringVar(&metricsAddr, "m", ":8100", "merics to use")
	flag.IntVar(&verbosityLevel, "v", -1, "verbosity level, higher value - more logs")
	help := flag.Bool("h", false, "help info")
	flag.Parse()
	if !load() {
		return false
	}

	if *help {
		return false
	}
	return true
}

func startMetrics(addr string) {
	// start metrics server
	m := http.NewServeMux()
	m.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Handler: m,
	}

	metricsLis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error(err, "cannot bind to metrics endpoint", "addr", addr)
		os.Exit(1)
	}
	logger.Info("Metrics Listening", "addr", addr)

	err = srv.Serve(metricsLis)
	if err != nil {
		logger.Error(err, "Metrics server stopped")
	}
}

func main() {

	if !parse() {
		showHelp()
		os.Exit(-1)
	}

	// Check that the -v is not set (default -1)
	if verbosityLevel < 0 {
		verbosityLevel = logConfig.Config.V
	}

	log.SetGlobalOptions(log.GlobalConfig{V: verbosityLevel})
	logger.Info("--- Starting SFU Node ---")

	// Pass logr instance
	sfu.Logger = logger
	s := sfu.NewSFU(conf)
	dc := s.NewDatachannel(sfu.APIChannelLabel)
	dc.Use(datachannel.SubscriberAPI)

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	//session, config := s.GetSession("A")
	//session.AddRelayPeer()

	http.Handle("/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			panic(err)
		}
		defer c.Close()

		p := server.NewJSONSignal(sfu.NewPeer(s), logger)
		defer p.Close()

		jc := jsonrpc2.NewConn(r.Context(), websocketjsonrpc2.NewObjectStream(c), p)
		<-jc.DisconnectNotify()
	}))

	http.Handle("/manual", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //todo: 일단 되는지 확인하기 위해서 정보를 가져오는 API와 relay를 trigger하는 API endpoint를 등록.
		logger.Info("Manual Received: method:" + r.Method)
		if r.Method == http.MethodGet {
			sessions := s.GetSessions()
			sessionInfoList := make([]sessionInfo, 0)
			for _, session := range sessions {
				peerIdList := make([]string, 0)
				peers := session.Peers()
				for _, peer := range peers {
					peerIdList = append(peerIdList, peer.ID())
				}
				sessionInfoList = append(sessionInfoList, sessionInfo{SessionId: session.ID(), PeerIdList: peerIdList})
			}

			res := &ResponseGet{
				Data: sessionInfoList,
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(res)
			return
		} else if r.Method == http.MethodPost {
			req := &RequestPost{}
			json.NewDecoder(r.Body).Decode(req)

			//session, _ := s.GetSession("test session")
			//
			//peers := session.Peers()
			//peerIDList := make([]string, 0)
			//for _, peer := range peers {
			//	peerIDList = append(peerIDList, peer.ID())
			//	if peer.ID() == "peer.ID()" {
			//		// RelayWithFanOutDataChannels
			//		r2, err := peer.Publisher().Relay(func(meta relay.PeerMeta, signal []byte) ([]byte, error) {
			//			fmt.Println("meta is: ", meta.String())
			//			fmt.Println("signal is:", string(signal))
			//			if meta.SessionID == "" {
			//				return nil, errors.New("not supported")
			//			}
			//			return signal, nil
			//		}, sfu.RelayWithFanOutDataChannels())
			//		if err != nil {
			//			fmt.Println("ERROR: ", err)
			//			continue
			//		}
			//		fmt.Println("r2: ", r2.ID())
			//	}
			//}
			res := &RequestPost{
				SessionId: req.SessionId + "-echo",
				PeerId:    req.PeerId + "-echo",
			}
			//
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(res)
			return

		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		//Peer.Publisher().Relay(...) then signal the data to the remote SFU and ingest the data using:
		//session.AddRelayPeer(peerID string, signalData []byte) ([]byte, error)
	}))

	go startMetrics(metricsAddr)

	var err error
	if key != "" && cert != "" {
		logger.Info("Started listening", "addr", "https://"+addr)
		err = http.ListenAndServeTLS(addr, cert, key, nil)
	} else {
		logger.Info("Started listening", "addr", "http://"+addr)
		err = http.ListenAndServe(addr, nil)
	}
	if err != nil {
		panic(err)
	}
}

type ResponsePost struct {
	Data []string `json:"data"`
}

type sessionInfo struct {
	SessionId  string   `json:"session_id"`
	PeerIdList []string `json:"peer_id_list"`
}
type ResponseGet struct {
	Data []sessionInfo `json:"data"`
}

type RequestPost struct {
	SessionId string `json:"session_id"`
	PeerId    string `json:"peer_id"`
}
