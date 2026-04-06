// matterbridge-signal-relay bridges signal-cli-rest-api and matterbridge's
// API gateway, enabling bidirectional Signal ↔ matterbridge message relay.
//
// Signal → matterbridge: connects to signal-cli-rest-api WebSocket,
// receives group messages, POSTs them to the matterbridge API.
//
// Matterbridge → Signal: streams from the matterbridge API, forwards
// messages to Signal groups via signal-cli-rest-api.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Config holds the relay configuration from environment variables.
type Config struct {
	SignalNumber    string            // e.g. "+15551234567"
	SignalAPI       string            // e.g. "http://signal-cli-rest-api:8080"
	MatterbridgeAPI string            // e.g. "http://localhost:4242"
	APIAccount      string            // matterbridge API account name, e.g. "api.signal"
	GatewayMap      map[string]string // gateway name → signal group ID
	SignalGroupMap  map[string]string // signal group ID → gateway name (reverse)
}

// signalEnvelope is the WebSocket message from signal-cli-rest-api.
type signalEnvelope struct {
	Envelope struct {
		Source     string `json:"source"`
		SourceUUID string `json:"sourceUuid"`
		SourceName string `json:"sourceName"`
		Timestamp  int64  `json:"timestamp"`
		DataMsg    *struct {
			Timestamp int64  `json:"timestamp"`
			Message   string `json:"message"`
			GroupInfo *struct {
				GroupID string `json:"groupId"`
				Type    string `json:"type"`
			} `json:"groupInfo"`
			GroupV2 *struct {
				ID       string `json:"id"`
				Revision int    `json:"revision"`
			} `json:"groupV2"`
		} `json:"dataMessage"`
	} `json:"envelope"`
	Account string `json:"account"`
}

// matterbridgeMessage is the matterbridge API message format.
type matterbridgeMessage struct {
	Text     string `json:"text"`
	Username string `json:"username"`
	Gateway  string `json:"gateway"`
	Event    string `json:"event,omitempty"`
	Account  string `json:"account,omitempty"`
}

// signalSendRequest is the signal-cli-rest-api v2 send format.
type signalSendRequest struct {
	Message    string   `json:"message"`
	Number     string   `json:"number"`
	Recipients []string `json:"recipients"`
}

func loadConfig() Config {
	signalNumber := os.Getenv("SIGNAL_NUMBER")
	if signalNumber == "" {
		log.Fatal("SIGNAL_NUMBER is required")
	}
	signalAPI := os.Getenv("SIGNAL_API")
	if signalAPI == "" {
		signalAPI = "http://signal-cli-rest-api:8080"
	}
	matterbridgeAPI := os.Getenv("MATTERBRIDGE_API")
	if matterbridgeAPI == "" {
		matterbridgeAPI = "http://localhost:4242"
	}
	apiAccount := os.Getenv("API_ACCOUNT")
	if apiAccount == "" {
		apiAccount = "api.signal"
	}
	gatewayMapStr := os.Getenv("GATEWAY_MAP")
	if gatewayMapStr == "" {
		log.Fatal("GATEWAY_MAP is required (e.g. my-gateway=group.ABC123,other=group.DEF456)")
	}

	gatewayMap := make(map[string]string)
	signalGroupMap := make(map[string]string)
	for _, pair := range strings.Split(gatewayMapStr, ",") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			log.Fatalf("invalid GATEWAY_MAP entry: %q", pair)
		}
		gateway := strings.TrimSpace(parts[0])
		groupID := strings.TrimSpace(parts[1])
		gatewayMap[gateway] = groupID
		signalGroupMap[groupID] = gateway
	}

	// Also build internal_id → gateway mapping by fetching group info
	// from signal-cli-rest-api. The WebSocket envelope uses internal_id
	// (raw base64) not the group.XXX format.
	internalIDs := fetchInternalIDs(signalAPI, signalNumber, signalGroupMap)
	for internalID, gateway := range internalIDs {
		signalGroupMap[internalID] = gateway
	}

	log.Printf("Config: signal=%s, api_account=%s, gateways=%d", signalNumber, apiAccount, len(gatewayMap))
	for gw, grp := range gatewayMap {
		log.Printf("  %s → %s", gw, grp)
	}

	return Config{
		SignalNumber:    signalNumber,
		SignalAPI:       signalAPI,
		MatterbridgeAPI: matterbridgeAPI,
		APIAccount:      apiAccount,
		GatewayMap:      gatewayMap,
		SignalGroupMap:  signalGroupMap,
	}
}

// signalGroup matches the signal-cli-rest-api /v1/groups response.
type signalGroup struct {
	Name       string `json:"name"`
	ID         string `json:"id"`
	InternalID string `json:"internal_id"`
}

// fetchInternalIDs queries signal-cli-rest-api for group info and builds
// a mapping from internal_id → gateway name. This is needed because
// signal-cli-rest-api uses "group.XXX" IDs in its REST API but the
// WebSocket envelope contains raw base64 internal IDs.
func fetchInternalIDs(signalAPI, signalNumber string, groupIDToGateway map[string]string) map[string]string {
	result := make(map[string]string)

	resp, err := http.Get(fmt.Sprintf("%s/v1/groups/%s", signalAPI, signalNumber))
	if err != nil {
		log.Printf("Warning: failed to fetch groups from signal-cli-rest-api: %v", err)
		return result
	}
	defer resp.Body.Close()

	var groups []signalGroup
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		log.Printf("Warning: failed to decode groups: %v", err)
		return result
	}

	for _, g := range groups {
		if gateway, ok := groupIDToGateway[g.ID]; ok {
			result[g.InternalID] = gateway
			log.Printf("  internal_id mapping: %s → %s (%s)", g.InternalID, gateway, g.Name)
		}
	}

	return result
}

// signalToMatterbridge connects to signal-cli WebSocket and forwards
// group messages to the matterbridge API.
func signalToMatterbridge(ctx context.Context, cfg Config, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := runSignalWebSocket(ctx, cfg)
		if err != nil {
			log.Printf("[signal→mb] WebSocket error: %v, reconnecting in 5s", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func runSignalWebSocket(ctx context.Context, cfg Config) error {
	wsURL, err := url.Parse(cfg.SignalAPI)
	if err != nil {
		return fmt.Errorf("parse signal API URL: %w", err)
	}
	wsURL.Scheme = "ws"
	wsURL.Path = fmt.Sprintf("/v1/receive/%s", cfg.SignalNumber)

	log.Printf("[signal→mb] Connecting to %s", wsURL.String())
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		return fmt.Errorf("dial websocket: %w", err)
	}
	defer conn.Close()
	log.Printf("[signal→mb] Connected")

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read websocket: %w", err)
		}

		var env signalEnvelope
		if err := json.Unmarshal(message, &env); err != nil {
			log.Printf("[signal→mb] Failed to parse message: %v", err)
			continue
		}

		// Skip non-data messages (receipts, typing indicators, etc.)
		if env.Envelope.DataMsg == nil || env.Envelope.DataMsg.Message == "" {
			continue
		}

		// Skip messages from ourselves (echo suppression)
		if env.Envelope.Source == cfg.SignalNumber {
			continue
		}

		// Determine the group ID
		groupID := ""
		if env.Envelope.DataMsg.GroupV2 != nil {
			groupID = env.Envelope.DataMsg.GroupV2.ID
		} else if env.Envelope.DataMsg.GroupInfo != nil {
			groupID = env.Envelope.DataMsg.GroupInfo.GroupID
		}

		if groupID == "" {
			continue
		}

		// Look up the gateway for this group
		gateway, ok := cfg.SignalGroupMap[groupID]
		if !ok {
			log.Printf("[signal→mb] Unknown group ID %s, ignoring", groupID)
			continue
		}

		// Build the sender name
		senderName := env.Envelope.SourceName
		if senderName == "" {
			senderName = env.Envelope.Source
		}

		mbMsg := matterbridgeMessage{
			Text:     env.Envelope.DataMsg.Message,
			Username: senderName,
			Gateway:  gateway,
			Account:  cfg.APIAccount,
		}

		if err := postToMatterbridge(cfg, mbMsg); err != nil {
			log.Printf("[signal→mb] Failed to post to matterbridge: %v", err)
		} else {
			log.Printf("[signal→mb] %s", gateway)
		}
	}
}

func postToMatterbridge(cfg Config, msg matterbridgeMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	resp, err := http.Post(cfg.MatterbridgeAPI+"/api/message", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// matterbridgeToSignal streams from the matterbridge API and forwards
// messages to Signal groups via signal-cli-rest-api.
func matterbridgeToSignal(ctx context.Context, cfg Config, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := runMatterbridgeStream(ctx, cfg)
		if err != nil {
			log.Printf("[mb→signal] Stream error: %v, reconnecting in 5s", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func runMatterbridgeStream(ctx context.Context, cfg Config) error {
	streamURL := cfg.MatterbridgeAPI + "/api/stream"
	log.Printf("[mb→signal] Connecting to %s", streamURL)

	req, err := http.NewRequestWithContext(ctx, "GET", streamURL, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	log.Printf("[mb→signal] Connected to stream")
	decoder := json.NewDecoder(resp.Body)

	for {
		var msg matterbridgeMessage
		if err := decoder.Decode(&msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("decode stream: %w", err)
		}

		// Skip messages that came FROM the signal API (echo suppression)
		if msg.Account == cfg.APIAccount {
			continue
		}

		// Skip empty messages
		if msg.Text == "" {
			continue
		}

		// Find the Signal group for this gateway
		groupID, ok := cfg.GatewayMap[msg.Gateway]
		if !ok {
			continue
		}

		// Username from the matterbridge stream already includes
		// the RemoteNickFormat, so just concatenate.
		text := msg.Username + " " + msg.Text

		if err := sendToSignal(cfg, groupID, text); err != nil {
			log.Printf("[mb→signal] Failed to send to Signal group %s: %v", msg.Gateway, err)
		} else {
			log.Printf("[mb→signal] %s", msg.Gateway)
		}
	}
}

func sendToSignal(cfg Config, groupID string, message string) error {
	reqBody := signalSendRequest{
		Message:    message,
		Number:     cfg.SignalNumber,
		Recipients: []string{groupID},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	resp, err := http.Post(cfg.SignalAPI+"/v2/send", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[signal-relay] ")

	cfg := loadConfig()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	var wg sync.WaitGroup

	wg.Add(1)
	go signalToMatterbridge(ctx, cfg, &wg)

	wg.Add(1)
	go matterbridgeToSignal(ctx, cfg, &wg)

	// Health check endpoint
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ok")
		})
		server := &http.Server{Addr: ":8081", Handler: mux}
		go func() {
			<-ctx.Done()
			_ = server.Shutdown(context.Background())
		}()
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("Health server error: %v", err)
		}
	}()

	log.Println("Signal relay started")
	wg.Wait()
	log.Println("Signal relay stopped")
}
