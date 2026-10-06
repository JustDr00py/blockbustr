package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// WebSocket /socket (TASKS P2.10, DESIGN §2 events), as captured from
// Jellyfin 12.1.0: the server opens with ForceKeepAlive (Data 60: send a
// KeepAlive at least every 60s), echoes every client KeepAlive, pushes
// UserDataChanged to the user's connections and LibraryChanged to everyone,
// and says ServerShuttingDown on the way out. Messages are PascalCase JSON
// {MessageId, Data, MessageType} for every client, as captured. The socket
// resolves its token itself: a refused upgrade is 403, not requireUser's 401.

const (
	keepAliveSeconds = 60
	socketIdle       = 2 * keepAliveSeconds * time.Second // silent this long: closed
	socketSendBuffer = 64
)

// Hub tracks WebSocket connections and turns events into messages.
type Hub struct {
	mu    sync.Mutex
	conns map[*wsConn]struct{}
	// render builds an event's message, or nil to skip it (set by Register).
	render func(ctx context.Context, e events.Event) []byte
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{conns: map[*wsConn]struct{}{}} }

type wsConn struct {
	user   uuid.UUID
	send   chan []byte
	cancel context.CancelFunc
}

func (h *Hub) add(c *wsConn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *wsConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// deliver queues msg on every connection keep accepts; a connection too far
// behind loses the message rather than holding up the others.
func (h *Hub) deliver(msg []byte, keep func(*wsConn) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if keep(c) {
			select {
			case c.send <- msg:
			default:
			}
		}
	}
}

// Connections is the number of open sockets.
func (h *Hub) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Run delivers events from bus until ctx ends.
func (h *Hub) Run(ctx context.Context, bus *events.Bus) error {
	evs, err := bus.Subscribe(ctx)
	if err != nil {
		return err
	}
	for e := range evs {
		if h.render == nil {
			continue
		}
		msg := h.render(ctx, e)
		if msg == nil {
			continue
		}
		switch e.Kind {
		case events.UserDataChanged:
			h.deliver(msg, func(c *wsConn) bool { return c.user == e.UserID })
		default:
			h.deliver(msg, func(*wsConn) bool { return true })
		}
	}
	return nil
}

// Shutdown tells every client the server is going away and closes the
// sockets (http.Server.Shutdown doesn't close hijacked connections).
func (h *Hub) Shutdown() {
	h.deliver(socketMessage("ServerShuttingDown", ""), func(*wsConn) bool { return true })
	time.Sleep(100 * time.Millisecond) // let the writers flush it
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		c.cancel()
	}
}

// socketMessage is one server message; data nil leaves Data out (KeepAlive).
func socketMessage(kind string, data any) []byte {
	m := map[string]any{"MessageId": dto.IDFromUUID(uuid.New()).String(), "MessageType": kind}
	if data != nil {
		m["Data"] = data
	}
	b, _ := json.Marshal(m)
	return b
}

func (a *api) registerSocket(rt *jfapi.Router) {
	if a.Hub == nil {
		return
	}
	a.Hub.render = a.renderEvent
	rt.Get("/socket", a.socket)
	rt.Get("/embywebsocket", a.socket) // older clients
}

func (a *api) socket(w http.ResponseWriter, r *http.Request) {
	s, err := a.Auth.Resolve(r.Context(), jfapi.AuthFrom(r.Context()).Token)
	if err != nil { // as captured: 403 with Jellyfin's text
		errorText(w, http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}) // authenticated by token, not cookies
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	c := &wsConn{user: s.UserID, send: make(chan []byte, socketSendBuffer), cancel: cancel}
	a.Hub.add(c)
	defer a.Hub.remove(c)
	a.touchSession(r, s)

	c.send <- socketMessage("ForceKeepAlive", keepAliveSeconds)
	go func() { // writer
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-c.send:
				wctx, done := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Write(wctx, websocket.MessageText, msg)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		rctx, done := context.WithTimeout(ctx, socketIdle)
		_, data, err := ws.Read(rctx)
		done()
		if err != nil {
			break
		}
		var in struct{ MessageType string }
		if json.Unmarshal(data, &in) != nil {
			continue
		}
		if in.MessageType == "KeepAlive" {
			a.touchSession(r, s)
			select {
			case c.send <- socketMessage("KeepAlive", nil):
			default:
			}
		}
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// renderEvent turns an event into its WebSocket message.
func (a *api) renderEvent(ctx context.Context, e events.Event) []byte {
	switch e.Kind {
	case events.UserDataChanged:
		list, err := a.userDataList(ctx, e.UserID, e.ItemIDs)
		if err != nil || len(list) == 0 {
			if err != nil {
				a.Log.WarnContext(ctx, "user data event", "err", err)
			}
			return nil
		}
		uid := dto.IDFromUUID(e.UserID)
		return socketMessage("UserDataChanged", dto.UserDataChangeInfo{UserId: &uid, UserDataList: list})
	case events.LibraryChanged:
		ids := func(in []uuid.UUID) *[]string {
			out := make([]string, len(in))
			for i, id := range in {
				out[i] = dto.IDFromUUID(id).String()
			}
			return &out
		}
		folders, err := a.libraryFolders(ctx, e.Libraries)
		if err != nil {
			a.Log.WarnContext(ctx, "library event", "err", err)
			return nil
		}
		return socketMessage("LibraryChanged", dto.LibraryUpdateInfo{
			FoldersAddedTo: &[]string{}, FoldersRemovedFrom: &[]string{}, ItemsAdded: &[]string{}, ItemsRemoved: &[]string{},
			ItemsUpdated: ids(e.Updated), CollectionFolders: ids(folders), IsEmpty: ptr(false),
		})
	}
	return nil
}

// userDataList is the user's data for the items and their season and
// series (whose played/unplayed counts change with an episode's), as
// Jellyfin lists it in UserDataChanged.
func (a *api) userDataList(ctx context.Context, user uuid.UUID, ids []uuid.UUID) ([]dto.UserItemDataDto, error) {
	items, err := a.itemsInIDOrder(ctx, ids)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	parents, err := a.loadParents(ctx, items)
	if err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	var all []db.Item
	addItem := func(it db.Item) {
		if !seen[it.ID] && it.Type != "CollectionFolder" {
			seen[it.ID] = true
			all = append(all, it)
		}
	}
	for _, it := range items {
		addItem(it)
		for p := it.ParentID; p != nil; {
			parent, ok := parents[*p]
			if !ok {
				break
			}
			addItem(parent)
			p = parent.ParentID
		}
	}
	dtos, err := a.itemDtos(ctx, &user, all, dtoOptions{fields: map[string]bool{}, imageTypeLimit: -1, enableUserData: true})
	if err != nil {
		return nil, err
	}
	out := make([]dto.UserItemDataDto, 0, len(dtos))
	for _, d := range dtos {
		if d.UserData != nil {
			out = append(out, *d.UserData)
		}
	}
	return out, nil
}

// libraryFolders are the CollectionFolder item ids of libraries.
func (a *api) libraryFolders(ctx context.Context, libs []uuid.UUID) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	if len(libs) == 0 {
		return out, nil
	}
	rows, err := a.DB.Query(ctx, `SELECT id FROM items WHERE type = 'CollectionFolder' AND library_id = ANY($1)`, libs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
