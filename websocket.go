package main

import (
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/thrasher-/gocryptotrader/common"
	"github.com/thrasher-/gocryptotrader/config"
	"github.com/thrasher-/gocryptotrader/currency"
)

// Const vars for websocket
const (
	WebsocketResponseSuccess = "OK"
)

var (
	wsHub        *WebsocketHub
	wsHubStarted bool
)

type wsCommandHandler struct {
	authRequired bool
	handler      func(client *WebsocketClient, data interface{}) error
}

var wsHandlers = map[string]wsCommandHandler{
	"auth":             wsCommandHandler{authRequired: false, handler: wsAuth},
	"getconfig":        wsCommandHandler{authRequired: true, handler: wsGetConfig},
	"saveconfig":       wsCommandHandler{authRequired: true, handler: wsSaveConfig},
	"getaccountinfo":   wsCommandHandler{authRequired: true, handler: wsGetAccountInfo},
	"gettickers":       wsCommandHandler{authRequired: false, handler: wsGetTickers},
	"getticker":        wsCommandHandler{authRequired: false, handler: wsGetTicker},
	"getorderbooks":    wsCommandHandler{authRequired: false, handler: wsGetOrderbooks},
	"getorderbook":     wsCommandHandler{authRequired: false, handler: wsGetOrderbook},
	"getexchangerates": wsCommandHandler{authRequired: false, handler: wsGetExchangeRates},
	"getportfolio":     wsCommandHandler{authRequired: true, handler: wsGetPortfolio},
}

// WebsocketClient stores information related to the websocket client
type WebsocketClient struct {
	Hub           *WebsocketHub
	Conn          *websocket.Conn
	Authenticated bool
	authFailures  int
	Send          chan []byte
}

// WebsocketHub stores the data for managing websocket clients
type WebsocketHub struct {
	Clients    map[*WebsocketClient]bool
	Broadcast  chan []byte
	Register   chan *WebsocketClient
	Unregister chan *WebsocketClient
}

// WebsocketEvent is the struct used for websocket events
type WebsocketEvent struct {
	Exchange  string `json:"exchange,omitempty"`
	AssetType string `json:"assetType,omitempty"`
	Event     string
	Data      interface{}
}

// WebsocketEventResponse is the struct used for websocket event responses
type WebsocketEventResponse struct {
	Event string      `json:"event"`
	Data  interface{} `json:"data"`
	Error string      `json:"error"`
}

// WebsocketOrderbookTickerRequest is a struct used for ticker and orderbook
// requests
type WebsocketOrderbookTickerRequest struct {
	Exchange  string `json:"exchangeName"`
	Currency  string `json:"currency"`
	AssetType string `json:"assetType"`
}

// WebsocketAuth is a struct used for
type WebsocketAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// NewWebsocketHub Creates a new websocket hub
func NewWebsocketHub() *WebsocketHub {
	return &WebsocketHub{
		Broadcast:  make(chan []byte),
		Register:   make(chan *WebsocketClient),
		Unregister: make(chan *WebsocketClient),
		Clients:    make(map[*WebsocketClient]bool),
	}
}

func (h *WebsocketHub) run() {
	for {
		select {
		case client := <-h.Register:
			h.Clients[client] = true
		case client := <-h.Unregister:
			if _, ok := h.Clients[client]; ok {
				log.Printf("websocket: disconnected client")
				delete(h.Clients, client)
				close(client.Send)
			}
		case message := <-h.Broadcast:
			for client := range h.Clients {
				select {
				case client.Send <- message:
				default:
					log.Printf("websocket: disconnected client")
					close(client.Send)
					delete(h.Clients, client)
				}
			}
		}
	}
}

// SendWebsocketMessage sends a websocket event to the client
func (c *WebsocketClient) SendWebsocketMessage(evt interface{}) error {
	data, err := common.JSONEncode(evt)
	if err != nil {
		log.Printf("websocket: failed to send message: %s", err)
		return err
	}

	c.Send <- data
	return nil
}

func (c *WebsocketClient) read() {
	defer func() {
		c.Hub.Unregister <- c
		c.Conn.Close()
	}()

	for {
		msgType, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("websocket: client disconnected, err: %s", err)
			}
			break
		}

		if msgType == websocket.TextMessage {
			var evt WebsocketEvent
			err := common.JSONDecode(message, &evt)
			if err != nil {
				log.Printf("websocket: failed to decode JSON sent from client %s", err)
				break
			}

			if evt.Event == "" {
				log.Printf("websocket: client sent a blank event, disconnecting")
				break
			}

			dataJSON, err := common.JSONEncode(evt.Data)
			if err != nil {
				log.Printf("websocket: client sent data we couldn't JSON decode")
				break
			}

			req := common.StringToLower(evt.Event)
			log.Printf("websocket: request received: %s", req)

			result, ok := wsHandlers[req]
			if !ok {
				log.Printf("websocket: unsupported event")
				continue
			}

			if result.authRequired && !c.Authenticated {
				log.Printf("Websocket: request %s failed due to unauthenticated request on an authenticated API", evt.Event)
				c.SendWebsocketMessage(WebsocketEventResponse{Event: evt.Event, Error: "unauthorised request on authenticated API"})
				continue
			}

			err = result.handler(c, dataJSON)
			if err != nil {
				log.Printf("websocket: request %s failed. Error %s", evt.Event, err)
				continue
			}
		}
	}
}

func (c *WebsocketClient) write() {
	defer func() {
		c.Conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				log.Printf("websocket: hub closed the channel")
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				log.Printf("websocket: failed to create new io.writeCloser: %s", err)
				return
			}
			w.Write(message)

			// Add queued chat messages to the current websocket message
			n := len(c.Send)
			for i := 0; i < n; i++ {
				w.Write(<-c.Send)
			}

			if err := w.Close(); err != nil {
				log.Printf("websocket: failed to close io.WriteCloser: %s", err)
				return
			}
		}
	}
}

// StartWebsocketHandler starts the websocket hub and routine which
// handles clients
func StartWebsocketHandler() {
	if !wsHubStarted {
		wsHubStarted = true
		wsHub = NewWebsocketHub()
		go wsHub.run()
	}
}

// BroadcastWebsocketMessage meow
func BroadcastWebsocketMessage(evt WebsocketEvent) error {
	data, err := common.JSONEncode(evt)
	if err != nil {
		return err
	}

	wsHub.Broadcast <- data
	return nil
}

// WebsocketClientHandler upgrades the HTTP connection to a websocket
// compatible one
func WebsocketClientHandler(w http.ResponseWriter, r *http.Request) {
	if !wsHubStarted {
		StartWebsocketHandler()
	}

	connectionLimit := bot.config.Webserver.WebsocketConnectionLimit
	numClients := len(wsHub.Clients)

	if numClients >= connectionLimit {
		log.Printf("websocket: client rejected due to websocket client limit reached. Number of clients %d. Limit %d.",
			numClients, connectionLimit)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	upgrader := websocket.Upgrader{
		WriteBufferSize: 1024,
		ReadBufferSize:  1024,
	}

	// Allow insecure origin if the Origin request header is present and not
	// equal to the Host request header. Default to false
	if bot.config.Webserver.WebsocketAllowInsecureOrigin {
		upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	client := &WebsocketClient{Hub: wsHub, Conn: conn, Send: make(chan []byte, 1024)}
	client.Hub.Register <- client
	log.Printf("websocket: client connected. Connected clients: %d. Limit %d.",
		numClients+1, connectionLimit)

	go client.read()
	go client.write()
}

func wsAuth(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "auth",
	}

	var auth WebsocketAuth
	err := common.JSONDecode(data.([]byte), &auth)
	if err != nil {
		wsResp.Error = err.Error()
		client.SendWebsocketMessage(wsResp)
		return err
	}

	hashPW := common.HexEncodeToString(common.GetSHA256([]byte(bot.config.Webserver.AdminPassword)))
	if auth.Username == bot.config.Webserver.AdminUsername && auth.Password == hashPW {
		client.Authenticated = true
		wsResp.Data = WebsocketResponseSuccess
		log.Println("websocket: client authenticated successfully")
		return client.SendWebsocketMessage(wsResp)
	} else {
		wsResp.Error = "invalid username/password"
		client.authFailures++
		client.SendWebsocketMessage(wsResp)
		if client.authFailures >= bot.config.Webserver.WebsocketMaxAuthFailures {
			log.Printf("websocket: disconnecting client, maximum auth failures threshold reached (failures: %d limit: %d)",
				client.authFailures, bot.config.Webserver.WebsocketMaxAuthFailures)
			wsHub.Unregister <- client
			return nil
		}

		log.Printf("websocket: client sent wrong username/password (failures: %d limit: %d)",
			client.authFailures, bot.config.Webserver.WebsocketMaxAuthFailures)
		return nil
	}
}

func wsGetConfig(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "GetConfig",
		Data:  bot.config,
	}
	return client.SendWebsocketMessage(wsResp)
}

func wsSaveConfig(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "SaveConfig",
	}
	var cfg config.Config
	err := common.JSONDecode(data.([]byte), &cfg)
	if err != nil {
		wsResp.Error = err.Error()
		client.SendWebsocketMessage(wsResp)
		return err
	}

	err = bot.config.UpdateConfig(bot.configFile, cfg)
	if err != nil {
		wsResp.Error = err.Error()
		client.SendWebsocketMessage(wsResp)
		return err
	}

	SetupExchanges()
	wsResp.Data = WebsocketResponseSuccess
	return client.SendWebsocketMessage(wsResp)
}

func wsGetAccountInfo(client *WebsocketClient, data interface{}) error {
	accountInfo := GetAllEnabledExchangeAccountInfo()
	wsResp := WebsocketEventResponse{
		Event: "GetAccountInfo",
		Data:  accountInfo,
	}
	return client.SendWebsocketMessage(wsResp)
}

func wsGetTickers(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "GetTickers",
	}
	wsResp.Data = GetAllActiveTickers()
	return client.SendWebsocketMessage(wsResp)
}

func wsGetTicker(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "GetTicker",
	}
	var tickerReq WebsocketOrderbookTickerRequest
	err := common.JSONDecode(data.([]byte), &tickerReq)
	if err != nil {
		wsResp.Error = err.Error()
		client.SendWebsocketMessage(wsResp)
		return err
	}

	result, err := GetSpecificTicker(tickerReq.Currency,
		tickerReq.Exchange, tickerReq.AssetType)

	if err != nil {
		wsResp.Error = err.Error()
		client.SendWebsocketMessage(wsResp)
		return err
	}
	wsResp.Data = result
	return client.SendWebsocketMessage(wsResp)
}

func wsGetOrderbooks(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "GetOrderbooks",
	}
	wsResp.Data = GetAllActiveOrderbooks()
	return client.SendWebsocketMessage(wsResp)
}

func wsGetOrderbook(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "GetOrderbook",
	}
	var orderbookReq WebsocketOrderbookTickerRequest
	err := common.JSONDecode(data.([]byte), &orderbookReq)
	if err != nil {
		wsResp.Error = err.Error()
		client.SendWebsocketMessage(wsResp)
		return err
	}

	result, err := GetSpecificOrderbook(orderbookReq.Currency,
		orderbookReq.Exchange, orderbookReq.AssetType)

	if err != nil {
		wsResp.Error = err.Error()
		client.SendWebsocketMessage(wsResp)
		return err
	}
	wsResp.Data = result
	return client.SendWebsocketMessage(wsResp)
}

func wsGetExchangeRates(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "GetExchangeRates",
	}
	wsResp.Data = currency.GetExchangeRates()
	return client.SendWebsocketMessage(wsResp)
}

func wsGetPortfolio(client *WebsocketClient, data interface{}) error {
	wsResp := WebsocketEventResponse{
		Event: "GetPortfolio",
	}
	wsResp.Data = bot.portfolio.GetPortfolioSummary()
	return client.SendWebsocketMessage(wsResp)
}
