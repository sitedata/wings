package websocket

import (
	"encoding/json"
	"fmt"
	"github.com/apex/log"
	"github.com/gbrlsnchs/jwt/v3"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	PermissionConnect          = "websocket.connect"
	PermissionSendCommand      = "control.console"
	PermissionSendPowerStart   = "control.start"
	PermissionSendPowerStop    = "control.stop"
	PermissionSendPowerRestart = "control.restart"
	PermissionReceiveErrors    = "admin.websocket.errors"
	PermissionReceiveInstall   = "admin.websocket.install"
	PermissionReceiveBackups   = "backup.read"
)

type Handler struct {
	sync.RWMutex
	Connection *websocket.Conn
	jwt        *tokens.WebsocketPayload `json:"-"`
	server     *server.Server
}

// Parses a JWT into a websocket token payload.
func NewTokenPayload(token []byte) (*tokens.WebsocketPayload, error) {
	payload := tokens.WebsocketPayload{}
	err := tokens.ParseToken(token, &payload)
	if err != nil {
		return nil, err
	}

	if !payload.HasPermission(PermissionConnect) {
		return nil, errors.New("not authorized to connect to this socket")
	}

	return &payload, nil
}

// Returns a new websocket handler using the context provided.
func GetHandler(s *server.Server, w http.ResponseWriter, r *http.Request) (*Handler, error) {
	upgrader := websocket.Upgrader{
		// Ensure that the websocket request is originating from the Panel itself,
		// and not some other location.
		CheckOrigin: func(r *http.Request) bool {
			return r.Header.Get("Origin") == config.Get().PanelLocation
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}

	return &Handler{
		Connection: conn,
		jwt:        nil,
		server:     s,
	}, nil
}

func (h *Handler) SendJson(v *Message) error {
	// Do not send JSON down the line if the JWT on the connection is not
	// valid!
	if err := h.TokenValid(); err != nil {
		return nil
	}

	j := h.GetJwt()
	if j != nil {
		// If we're sending installation output but the user does not have the required
		// permissions to see the output, don't send it down the line.
		if v.Event == server.InstallOutputEvent {
			if !j.HasPermission(PermissionReceiveInstall) {
				return nil
			}
		}

		// If the user does not have permission to see backup events, do not emit
		// them over the socket.
		if strings.HasPrefix(v.Event, server.BackupCompletedEvent) {
			if !j.HasPermission(PermissionReceiveBackups) {
				return nil
			}
		}
	}

	return h.unsafeSendJson(v)
}

// Sends JSON over the websocket connection, ignoring the authentication state of the
// socket user. Do not call this directly unless you are positive a response should be
// sent back to the client!
func (h *Handler) unsafeSendJson(v interface{}) error {
	h.Lock()
	defer h.Unlock()

	return h.Connection.WriteJSON(v)
}

// Checks if the JWT is still valid.
func (h *Handler) TokenValid() error {
	j := h.GetJwt()
	if j == nil {
		return errors.New("no jwt present")
	}

	if err := jwt.ExpirationTimeValidator(time.Now())(&j.Payload); err != nil {
		return err
	}

	if !j.HasPermission(PermissionConnect) {
		return errors.New("jwt does not have connect permission")
	}

	if h.server.Uuid != j.ServerUUID {
		return errors.New("jwt server uuid mismatch")
	}

	return nil
}

// Sends an error back to the connected websocket instance by checking the permissions
// of the token. If the user has the "receive-errors" grant we will send back the actual
// error message, otherwise we just send back a standard error message.
func (h *Handler) SendErrorJson(msg Message, err error) error {
	j := h.GetJwt()

	message := "an unexpected error was encountered while handling this request"
	if server.IsSuspendedError(err) || (j != nil && j.HasPermission(PermissionReceiveErrors)) {
		message = err.Error()
	}

	m, u := h.GetErrorMessage(message)

	wsm := Message{Event: ErrorEvent}
	wsm.Args = []string{m}

	if !server.IsSuspendedError(err) {
		h.server.Log().WithFields(log.Fields{"event": msg.Event, "error_identifier": u.String(), "error": err}).
			Error("failed to handle websocket process; an error was encountered processing an event")
	}

	return h.unsafeSendJson(wsm)
}

// Converts an error message into a more readable representation and returns a UUID
// that can be cross-referenced to find the specific error that triggered.
func (h *Handler) GetErrorMessage(msg string) (string, uuid.UUID) {
	u := uuid.Must(uuid.NewRandom())

	m := fmt.Sprintf("Error Event [%s]: %s", u.String(), msg)

	return m, u
}

// Sets the JWT for the websocket in a race-safe manner.
func (h *Handler) setJwt(token *tokens.WebsocketPayload) {
	h.Lock()
	h.jwt = token
	h.Unlock()
}

func (h *Handler) GetJwt() *tokens.WebsocketPayload {
	h.RLock()
	defer h.RUnlock()

	return h.jwt
}

// Handle the inbound socket request and route it to the proper server action.
func (h *Handler) HandleInbound(m Message) error {
	if m.Event != AuthenticationEvent {
		if err := h.TokenValid(); err != nil {
			log.WithField("message", err.Error()).Debug("jwt for server websocket is no longer valid")

			h.unsafeSendJson(Message{
				Event: ErrorEvent,
				Args:  []string{"could not authenticate client: " + err.Error()},
			})

			return nil
		}
	}

	switch m.Event {
	case AuthenticationEvent:
		{
			token, err := NewTokenPayload([]byte(strings.Join(m.Args, "")))
			if err != nil {
				// If the error says the JWT expired, send a token expired
				// event and hopefully the client renews the token.
				if err == jwt.ErrExpValidation {
					h.SendJson(&Message{Event: TokenExpiredEvent})
					return nil
				}

				return err
			}

			// Check if the user has previously authenticated successfully.
			newConnection := h.GetJwt() == nil

			// Previously there was a HasPermission(PermissionConnect) check around this,
			// however NewTokenPayload will return an error if it doesn't have the connect
			// permission meaning that it was a redundant function call.
			h.setJwt(token)

			// Tell the client they authenticated successfully.
			h.unsafeSendJson(Message{
				Event: AuthenticationSuccessEvent,
				Args:  []string{},
			})

			// Check if the client was refreshing their authentication token
			// instead of authenticating for the first time.
			if !newConnection {
				// This prevents duplicate status messages as outlined in
				// https://github.com/pterodactyl/panel/issues/2077
				return nil
			}

			// On every authentication event, send the current server status back
			// to the client. :)
			state := h.server.GetState()
			h.SendJson(&Message{
				Event: server.StatusEvent,
				Args:  []string{state},
			})

			// Only send the current disk usage if the server is offline, if docker container is running,
			// Environment#EnableResourcePolling() will send this data to all clients.
			if state == server.ProcessOfflineState {
				_ = h.server.Filesystem.HasSpaceAvailable()

				resources := server.ResourceUsage{
					Memory:      0,
					MemoryLimit: 0,
					CpuAbsolute: 0.0,
					Disk:        h.server.Resources.Disk,
				}
				resources.Network.RxBytes = 0
				resources.Network.TxBytes = 0

				b, _ := json.Marshal(resources)
				h.SendJson(&Message{
					Event: server.StatsEvent,
					Args:  []string{string(b)},
				})
			}

			return nil
		}
	case SetStateEvent:
		{
			switch strings.Join(m.Args, "") {
			case "start":
				if h.GetJwt().HasPermission(PermissionSendPowerStart) {
					return h.server.Environment.Start()
				}
				break
			case "stop":
				if h.GetJwt().HasPermission(PermissionSendPowerStop) {
					return h.server.Environment.Stop()
				}
				break
			case "restart":
				if h.GetJwt().HasPermission(PermissionSendPowerRestart) {
					if err := h.server.Environment.WaitForStop(60, false); err != nil {
						return err
					}

					return h.server.Environment.Start()
				}
				break
			case "kill":
				if h.GetJwt().HasPermission(PermissionSendPowerStop) {
					return h.server.Environment.Terminate(os.Kill)
				}
				break
			}

			return nil
		}
	case SendServerLogsEvent:
		{
			if running, _ := h.server.Environment.IsRunning(); !running {
				return nil
			}

			logs, err := h.server.Environment.Readlog(1024 * 16)
			if err != nil {
				return err
			}

			for _, line := range logs {
				h.SendJson(&Message{
					Event: server.ConsoleOutputEvent,
					Args:  []string{line},
				})
			}

			return nil
		}
	case SendCommandEvent:
		{
			if !h.GetJwt().HasPermission(PermissionSendCommand) {
				return nil
			}

			if h.server.GetState() == server.ProcessOfflineState {
				return nil
			}

			return h.server.Environment.SendCommand(strings.Join(m.Args, ""))
		}
	}

	return nil
}
