package nakama

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	nkapi "github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/rtapi"
	"golang.org/x/exp/maps"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"nhooyr.io/websocket"
)

// Handler is the interface for connection handlers.
type Handler interface {
	HttpClient() *http.Client
	SocketURL() (string, error)
	Token(context.Context) (string, error)
	Logf(string, ...interface{})
	Errf(string, ...interface{})
}

// Conn is a nakama realtime websocket connection.
type Conn struct {
	h      Handler
	url    string
	token  string
	binary bool
	query  url.Values
	conn   *websocket.Conn
	cancel func()
	out    chan *req
	in     chan []byte
	l      map[string]*req
	rw     sync.RWMutex
	id     uint64
}

// NewConn creates a new nakama realtime websocket connection.
func NewConn(ctx context.Context, opts ...ConnOption) (*Conn, error) {
	conn := &Conn{
		binary: true,
		query:  url.Values{},
		out:    make(chan *req),
		in:     make(chan []byte),
		l:      make(map[string]*req),
	}
	for _, o := range opts {
		o(conn)
	}
	// build url
	urlstr := conn.url
	if urlstr == "" && conn.h != nil {
		var err error
		if urlstr, err = conn.h.SocketURL(); err != nil {
			return nil, err
		}
	}
	// build token
	token := conn.token
	if token == "" && conn.h != nil {
		var err error
		if token, err = conn.h.Token(ctx); err != nil {
			return nil, err
		}
	}
	// build query
	query := url.Values{}
	for k, v := range conn.query {
		query[k] = v
	}
	query.Set("token", token)
	format := "protobuf"
	if !conn.binary {
		format = "json"
	}
	query.Set("format", format)
	httpClient := http.DefaultClient
	if conn.h != nil {
		httpClient = conn.h.HttpClient()
	}
	// open socket
	var err error
	conn.conn, _, err = websocket.Dial(ctx, urlstr+"?"+query.Encode(), &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to open nakama websocket %s: %w", urlstr, err)
	}
	// run
	ctx, conn.cancel = context.WithCancel(ctx)
	go conn.run(ctx)
	return conn, nil
}

// marshal marshals the message. If the format set on the connection is json,
// then the message will be marshaled using json encoding.
func (conn *Conn) marshal(env *rtapi.Envelope) ([]byte, error) {
	f := proto.Marshal
	if !conn.binary {
		f = protojson.Marshal
	}
	return f(env)
}

// unmarshal unmarshals the message. If the format set on the connection is
// json, then v will be unmarshaled using json encoding.
func (conn *Conn) unmarshal(buf []byte) (*rtapi.Envelope, error) {
	f := proto.Unmarshal
	if !conn.binary {
		f = protojson.Unmarshal
	}
	env := new(rtapi.Envelope)
	if err := f(buf, env); err != nil {
		return nil, err
	}
	return env, nil
}

// run handles incoming and outgoing websocket messages.
func (conn *Conn) run(ctx context.Context) {
	// read incoming
	go func() {
		for {
			select {
			case <-ctx.Done():
			default:
			}
			_, r, err := conn.conn.Reader(ctx)
			switch {
			case err != nil && (errors.Is(err, context.Canceled) || errors.As(err, &websocket.CloseError{})):
				return
			case err != nil:
				conn.h.Errf("reader error: %v", err)
				continue
			}
			buf, err := ioutil.ReadAll(r)
			if err != nil {
				conn.h.Errf("unable to read message: %v", err)
				continue
			}
			conn.in <- buf
		}
	}()
	// dispatch outgoing/incoming
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-conn.out:
			if m == nil {
				continue
			}
			id, err := conn.send(ctx, m.msg)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					conn.h.Errf("unable to send message: %v", err)
				}
				m.err <- fmt.Errorf("unable to send message: %w", err)
				close(m.err)
				continue
			}
			if m.v == nil || id == "" {
				close(m.err)
				continue
			}
			conn.rw.Lock()
			conn.l[id] = m
			conn.rw.Unlock()
		case buf := <-conn.in:
			if buf == nil {
				continue
			}
			if err := conn.recv(buf); err != nil {
				conn.h.Errf("unable to dispatch incoming message: %v", err)
				continue
			}
		}
	}
}

// send marshals the message and writes it to the websocket connection.
func (conn *Conn) send(ctx context.Context, msg EnvelopeBuilder) (string, error) {
	env := msg.BuildEnvelope()
	env.Cid = strconv.FormatUint(atomic.AddUint64(&conn.id, 1), 10)
	buf, err := conn.marshal(env)
	if err != nil {
		return "", err
	}
	typ := websocket.MessageBinary
	if !conn.binary {
		typ = websocket.MessageText
	}
	if err := conn.conn.Write(ctx, typ, buf); err != nil {
		return "", err
	}
	return env.Cid, nil
}

// recv unmarshals buf, dispatching the message.
func (conn *Conn) recv(buf []byte) error {
	env, err := conn.unmarshal(buf)
	switch {
	case err != nil:
		return fmt.Errorf("unable to unmarshal: %w", err)
	case env.Cid == "":
		return conn.recvNotify(env)
	}
	return conn.recvResponse(env)
}

// recvNotify dispaches events and received updates.
func (conn *Conn) recvNotify(env *rtapi.Envelope) error {
	switch v := env.Message.(type) {
	case *rtapi.Envelope_Error:
		conn.notifyError(v.Error)
		return NewRealtimeError(v.Error)
	case *rtapi.Envelope_ChannelMessage:
		conn.notifyChannelMessage(v.ChannelMessage)
	case *rtapi.Envelope_ChannelPresenceEvent:
		conn.notifyChannelPresenceEvent(v.ChannelPresenceEvent)
	case *rtapi.Envelope_MatchData:
		conn.notifyMatchData(v.MatchData)
	case *rtapi.Envelope_MatchPresenceEvent:
		conn.notifyMatchPresenceEvent(v.MatchPresenceEvent)
	case *rtapi.Envelope_MatchmakerMatched:
		conn.notifyMatchmakerMatched(v.MatchmakerMatched)
	case *rtapi.Envelope_Notifications:
		conn.notifyNotifications(v.Notifications)
	case *rtapi.Envelope_StatusPresenceEvent:
		conn.notifyStatusPresenceEvent(v.StatusPresenceEvent)
	case *rtapi.Envelope_StreamData:
		conn.notifyStreamData(v.StreamData)
	case *rtapi.Envelope_StreamPresenceEvent:
		conn.notifyStreamPresenceEvent(v.StreamPresenceEvent)
	default:
		return fmt.Errorf("unknown type %T", env.Message)
	}
	return nil
}

// recvResponse dispatches a received a response (messages with cid != "").
func (conn *Conn) recvResponse(env *rtapi.Envelope) error {
	conn.rw.RLock()
	req, ok := conn.l[env.Cid]
	conn.rw.RUnlock()
	if !ok || req == nil {
		return fmt.Errorf("no callback id %s (%T)", env.Cid, env.Message)
	}
	// remove and close
	defer func() {
		close(req.err)
		conn.rw.Lock()
		delete(conn.l, env.Cid)
		conn.rw.Unlock()
	}()
	// check error
	switch v := env.Message.(type) {
	case *rtapi.Envelope_Error:
		conn.h.Logf("Error: %+v", v.Error)
		req.err <- NewRealtimeError(v.Error)
		return nil
	case nil:
		conn.h.Logf("Empty, Cid: %s", env.Cid)
	case *rtapi.Envelope_Channel:
		conn.h.Logf("Channel: %+v, Cid: %s", v.Channel, env.Cid)
	case *rtapi.Envelope_ChannelMessageAck:
		conn.h.Logf("ChannelMessageAck: %+v, Cid: %s", v.ChannelMessageAck, env.Cid)
	case *rtapi.Envelope_MatchmakerTicket:
		conn.h.Logf("MatchmakerTicket: %+v, Cid: %s", v.MatchmakerTicket, env.Cid)
	case *rtapi.Envelope_Pong:
		conn.h.Logf("Pong, Cid: %s", env.Cid)
	case *rtapi.Envelope_Status:
		conn.h.Logf("Status: %+v, Cid: %s", v.Status, env.Cid)
	case *rtapi.Envelope_Rpc:
		conn.h.Logf("Rpc: %+v, Cid: %s", v.Rpc, env.Cid)
	default:
		return fmt.Errorf("unknown type %T cid: %s", env.Message, env.Cid)
	}
	// merge
	proto.Merge(req.v.BuildEnvelope(), env)
	return nil
}

// Send sends a message.
func (conn *Conn) Send(ctx context.Context, msg, v EnvelopeBuilder) error {
	m := &req{
		msg: msg,
		v:   v,
		err: make(chan error, 1),
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case conn.out <- m:
	}
	var err error
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err = <-m.err:
	}
	return err
}

// Close closes the websocket connection.
func (conn *Conn) Close() error {
	if conn.cancel != nil {
		defer conn.cancel()
	}
	if conn.conn != nil {
		return conn.conn.Close(websocket.StatusGoingAway, "going away")
	}
	return nil
}

func (conn *Conn) notifyError(msg *rtapi.Error) {
}

func (conn *Conn) notifyChannelMessage(msg *nkapi.ChannelMessage) {
}

func (conn *Conn) notifyChannelPresenceEvent(msg *rtapi.ChannelPresenceEvent) {
}

func (conn *Conn) notifyMatchData(msg *rtapi.MatchData) {
}

func (conn *Conn) notifyMatchPresenceEvent(msg *rtapi.MatchPresenceEvent) {
}

func (conn *Conn) notifyMatchmakerMatched(msg *rtapi.MatchmakerMatched) {
}

func (conn *Conn) notifyNotifications(msg *rtapi.Notifications) {
}

func (conn *Conn) notifyStatusPresenceEvent(msg *rtapi.StatusPresenceEvent) {
}

func (conn *Conn) notifyStreamData(msg *rtapi.StreamData) {
}

func (conn *Conn) notifyStreamPresenceEvent(msg *rtapi.StreamPresenceEvent) {
}

// ChannelJoin sends a message to join a chat channel.
func (conn *Conn) ChannelJoin(ctx context.Context, target string, typ ChannelJoinType, persistence, hidden bool) (*ChannelMsg, error) {
	return ChannelJoin(target, typ).
		WithPersistence(persistence).
		WithHidden(hidden).
		Send(ctx, conn)
}

// ChannelJoinAsync sends a message to join a chat channel.
func (conn *Conn) ChannelJoinAsync(ctx context.Context, target string, typ ChannelJoinType, persistence, hidden bool, f func(*ChannelMsg, error)) {
	ChannelJoin(target, typ).
		WithPersistence(persistence).
		WithHidden(hidden).
		Async(ctx, conn, f)
}

// ChannelLeave sends a message to leave a chat channel.
func (conn *Conn) ChannelLeave(ctx context.Context, channelId string) error {
	return ChannelLeave(channelId).Send(ctx, conn)
}

// ChannelLeaveAsync sends a message to leave a chat channel.
func (conn *Conn) ChannelLeaveAsync(ctx context.Context, channelId string, f func(error)) {
	ChannelLeave(channelId).Async(ctx, conn, f)
}

// ChannelMessageRemove sends a message to remove a message from a channel.
func (conn *Conn) ChannelMessageRemove(ctx context.Context, channelId, messageId string) (*ChannelMessageAckMsg, error) {
	return ChannelMessageRemove(channelId, messageId).Send(ctx, conn)
}

// ChannelMessageRemoveAsync sends a message to remove a message from a channel.
func (conn *Conn) ChannelMessageRemoveAsync(ctx context.Context, channelId, messageId string, f func(*ChannelMessageAckMsg, error)) {
	ChannelMessageRemove(channelId, messageId).Async(ctx, conn, f)
}

// ChannelMessageSend sends a message on a channel.
func (conn *Conn) ChannelMessageSend(ctx context.Context, channelId, content string) (*ChannelMessageAckMsg, error) {
	return ChannelMessageSend(channelId, content).Send(ctx, conn)
}

// ChannelMessageSendAsync sends a message on a channel.
func (conn *Conn) ChannelMessageSendAsync(ctx context.Context, channelId, content string, f func(*ChannelMessageAckMsg, error)) {
	ChannelMessageSend(channelId, content).Async(ctx, conn, f)
}

// ChannelMessageUpdate sends a message to update a message on a channel.
func (conn *Conn) ChannelMessageUpdate(ctx context.Context, channelId, messageId, content string) (*ChannelMessageAckMsg, error) {
	return ChannelMessageUpdate(channelId, messageId, content).Send(ctx, conn)
}

// ChannelMessageUpdateAsync sends a message to update a message on a channel.
func (conn *Conn) ChannelMessageUpdateAsync(ctx context.Context, channelId, messageId, content string, f func(*ChannelMessageAckMsg, error)) {
	ChannelMessageUpdate(channelId, messageId, content).Async(ctx, conn, f)
}

// MatchCreate sends a message to create a multiplayer match.
func (conn *Conn) MatchCreate(ctx context.Context, name string) (*MatchMsg, error) {
	return MatchCreate(name).Send(ctx, conn)
}

// MatchCreateAsync sends a message to create a multiplayer match.
func (conn *Conn) MatchCreateAsync(ctx context.Context, name string, f func(*MatchMsg, error)) {
	MatchCreate(name).Async(ctx, conn, f)
}

// MatchJoin sends a message to join a match.
func (conn *Conn) MatchJoin(ctx context.Context, matchId string, metadata map[string]string) (*MatchMsg, error) {
	return MatchJoin(matchId).
		WithMetadata(metadata).
		Send(ctx, conn)
}

// MatchJoinAsync sends a message to join a match.
func (conn *Conn) MatchJoinAsync(ctx context.Context, matchId string, metadata map[string]string, f func(*MatchMsg, error)) {
	MatchJoin(matchId).
		WithMetadata(metadata).
		Async(ctx, conn, f)
}

// MatchJoinToken sends a message to join a match with a token.
func (conn *Conn) MatchJoinToken(ctx context.Context, token string, metadata map[string]string) (*MatchMsg, error) {
	return MatchJoinToken(token).
		WithMetadata(metadata).
		Send(ctx, conn)
}

// MatchJoinTokenAsync sends a message to join a match with a token.
func (conn *Conn) MatchJoinTokenAsync(ctx context.Context, token string, metadata map[string]string, f func(*MatchMsg, error)) {
	MatchJoinToken(token).
		WithMetadata(metadata).
		Async(ctx, conn, f)
}

// MatchLeave sends a message to leave a multiplayer match.
func (conn *Conn) MatchLeave(ctx context.Context, matchId string) error {
	return MatchLeave(matchId).Send(ctx, conn)
}

// MatchLeaveAsync sends a message to leave a multiplayer match.
func (conn *Conn) MatchLeaveAsync(ctx context.Context, matchId string, f func(error)) {
	MatchLeave(matchId).Async(ctx, conn, f)
}

// MatchmakerAdd sends a message to join the matchmaker pool and search for opponents on the server.
func (conn *Conn) MatchmakerAdd(ctx context.Context, msg *MatchmakerAddMsg) (*MatchmakerTicketMsg, error) {
	return msg.Send(ctx, conn)
}

// MatchmakerAddAsync sends a message to join the matchmaker pool and search for opponents on the server.
func (conn *Conn) MatchmakerAddAsync(ctx context.Context, msg *MatchmakerAddMsg, f func(*MatchmakerTicketMsg, error)) {
	msg.Async(ctx, conn, f)
}

// MatchmakerRemove sends a message to leave the matchmaker pool for a ticket.
func (conn *Conn) MatchmakerRemove(ctx context.Context, ticket string) error {
	return MatchmakerRemove(ticket).Send(ctx, conn)
}

// MatchmakerRemoveAsync sends a message to leave the matchmaker pool for a ticket.
func (conn *Conn) MatchmakerRemoveAsync(ctx context.Context, ticket string, f func(error)) {
	MatchmakerRemove(ticket).Async(ctx, conn, f)
}

// MatchDataSend sends a message to send input to a multiplayer match.
func (conn *Conn) MatchDataSend(ctx context.Context, matchId string, opCode OpType, data []byte, reliable bool, presences ...*UserPresenceMsg) error {
	return MatchDataSend(matchId, opCode, data).
		WithPresences(presences...).
		WithReliable(reliable).
		Send(ctx, conn)
}

// MatchDataSendAsync sends a message to send input to a multiplayer match.
func (conn *Conn) MatchDataSendAsync(ctx context.Context, matchId string, opCode OpType, data []byte, reliable bool, presences []*UserPresenceMsg, f func(error)) {
	MatchDataSend(matchId, opCode, data).
		WithPresences(presences...).
		WithReliable(reliable).
		Async(ctx, conn, f)
}

// PartyAccept sends a message to accept a party member.
func (conn *Conn) PartyAccept(ctx context.Context, partyId string, presence *UserPresenceMsg) error {
	return PartyAccept(partyId, presence).Send(ctx, conn)
}

// PartyAcceptAsync sends a message to accept a party member.
func (conn *Conn) PartyAcceptAsync(ctx context.Context, partyId string, presence *UserPresenceMsg, f func(error)) {
	PartyAccept(partyId, presence).Async(ctx, conn, f)
}

// PartyClose sends a message closes a party, kicking all party members.
func (conn *Conn) PartyClose(ctx context.Context, partyId string) error {
	return PartyClose(partyId).Send(ctx, conn)
}

// PartyCloseAsync sends a message closes a party, kicking all party members.
func (conn *Conn) PartyCloseAsync(ctx context.Context, partyId string, f func(error)) {
	PartyClose(partyId).Async(ctx, conn, f)
}

// PartyCreate sends a message to create a party.
func (conn *Conn) PartyCreate(ctx context.Context, open bool, maxSize int) (*PartyMsg, error) {
	return PartyCreate(open, maxSize).Send(ctx, conn)
}

// PartyCreateAsync sends a message to create a party.
func (conn *Conn) PartyCreateAsync(ctx context.Context, open bool, maxSize int, f func(*PartyMsg, error)) {
	PartyCreate(open, maxSize).Async(ctx, conn, f)
}

// PartyDataSend sends a message to send input to a multiplayer party.
func (conn *Conn) PartyDataSend(ctx context.Context, partyId string, opCode OpType, data []byte, reliable bool, presences ...*UserPresenceMsg) error {
	return PartyDataSend(partyId, opCode, data).Send(ctx, conn)
}

// PartyDataSendAsync sends a message to send input to a multiplayer party.
func (conn *Conn) PartyDataSendAsync(ctx context.Context, partyId string, opCode OpType, data []byte, reliable bool, presences []*UserPresenceMsg, f func(error)) {
	PartyDataSend(partyId, opCode, data).Async(ctx, conn, f)
}

// PartyJoin sends a message to join a party.
func (conn *Conn) PartyJoin(ctx context.Context, partyId string) error {
	return PartyJoin(partyId).Send(ctx, conn)
}

// PartyJoinAsync sends a message to join a party.
func (conn *Conn) PartyJoinAsync(ctx context.Context, partyId string, f func(error)) {
	PartyJoin(partyId).Async(ctx, conn, f)
}

// PartyJoinRequests sends a message to request the list of pending join requests for a party.
func (conn *Conn) PartyJoinRequests(ctx context.Context, partyId string) (*PartyJoinRequestMsg, error) {
	return PartyJoinRequests(partyId).Send(ctx, conn)
}

// PartyJoinRequestsAsync sends a message to request the list of pending join requests for a party.
func (conn *Conn) PartyJoinRequestsAsync(ctx context.Context, partyId string, f func(*PartyJoinRequestMsg, error)) {
	PartyJoinRequests(partyId).Async(ctx, conn, f)
}

// PartyLeave sends a message to leave a party.
func (conn *Conn) PartyLeave(ctx context.Context, partyId string) error {
	return PartyLeave(partyId).Send(ctx, conn)
}

// PartyLeaveAsync sends a message to leave a party.
func (conn *Conn) PartyLeaveAsync(ctx context.Context, partyId string, f func(error)) {
	PartyLeave(partyId).Async(ctx, conn, f)
}

// PartyMatchmakerAdd sends a message to begin matchmaking as a party.
func (conn *Conn) PartyMatchmakerAdd(ctx context.Context, partyId, query string, minCount, maxCount int) (*PartyMatchmakerTicketMsg, error) {
	return PartyMatchmakerAdd(partyId, query, minCount, maxCount).Send(ctx, conn)
}

// PartyMatchmakerAddAsync sends a message to begin matchmaking as a party.
func (conn *Conn) PartyMatchmakerAddAsync(ctx context.Context, partyId, query string, minCount, maxCount int, f func(*PartyMatchmakerTicketMsg, error)) {
	PartyMatchmakerAdd(partyId, query, minCount, maxCount).Async(ctx, conn, f)
}

// PartyMatchmakerRemove sends a message to cancel a party matchmaking process for a ticket.
func (conn *Conn) PartyMatchmakerRemove(ctx context.Context, partyId, ticket string) error {
	return PartyMatchmakerRemove(partyId, ticket).Send(ctx, conn)
}

// PartyMatchmakerRemoveAsync sends a message to cancel a party matchmaking process for a ticket.
func (conn *Conn) PartyMatchmakerRemoveAsync(ctx context.Context, partyId, ticket string, f func(error)) {
	PartyMatchmakerRemove(partyId, ticket).Async(ctx, conn, f)
}

// PartyPromote sends a message to promote a new party leader.
func (conn *Conn) PartyPromote(ctx context.Context, partyId string, presence *UserPresenceMsg) (*PartyLeaderMsg, error) {
	return PartyPromote(partyId, presence).Send(ctx, conn)
}

// PartyPromoteAsync sends a message to promote a new party leader.
func (conn *Conn) PartyPromoteAsync(ctx context.Context, partyId string, presence *UserPresenceMsg, f func(*PartyLeaderMsg, error)) {
	PartyPromote(partyId, presence).Async(ctx, conn, f)
}

// PartyRemove sends a message to kick a party member or decline a request to join.
func (conn *Conn) PartyRemove(ctx context.Context, partyId string, presence *UserPresenceMsg) error {
	return PartyRemove(partyId, presence).Send(ctx, conn)
}

// PartyRemoveAsync sends a message to kick a party member or decline a request to join.
func (conn *Conn) PartyRemoveAsync(ctx context.Context, partyId string, presence *UserPresenceMsg, f func(error)) {
	PartyRemove(partyId, presence).Async(ctx, conn, f)
}

// Ping sends a message to do a ping.
func (conn *Conn) Ping(ctx context.Context) error {
	return Ping().Send(ctx, conn)
}

// PingAsync sends a message to do a ping.
func (conn *Conn) PingAsync(ctx context.Context, f func(error)) {
	Ping().Async(ctx, conn, f)
}

// Rpc sends a message to execute a remote procedure call.
func (conn *Conn) Rpc(ctx context.Context, id string, payload, v interface{}) error {
	return Rpc(id, payload, v).Send(ctx, conn)
}

// RpcAsync sends a message to execute a remote procedure call.
func (conn *Conn) RpcAsync(ctx context.Context, id string, payload, v interface{}, f func(error)) {
	Rpc(id, payload, v).SendAsync(ctx, conn, f)
}

// StatusFollow sends a message to subscribe to user status updates.
func (conn *Conn) StatusFollow(ctx context.Context, userIds ...string) (*StatusMsg, error) {
	return StatusFollow(userIds...).Send(ctx, conn)
}

// StatusFollowAsync sends a message to subscribe to user status updates.
func (conn *Conn) StatusFollowAsync(ctx context.Context, userIds []string, f func(*StatusMsg, error)) {
	StatusFollow(userIds...).Async(ctx, conn, f)
}

// StatusUnfollow sends a message to unfollow user's status updates.
func (conn *Conn) StatusUnfollow(ctx context.Context, userIds ...string) error {
	return StatusUnfollow(userIds...).Send(ctx, conn)
}

// StatusUnfollowAsync sends a message to unfollow user's status updates.
func (conn *Conn) StatusUnfollowAsync(ctx context.Context, userIds []string, f func(error)) {
	StatusUnfollow(userIds...).Async(ctx, conn, f)
}

// StatusUpdate sends a message to update the user's status.
func (conn *Conn) StatusUpdate(ctx context.Context, status string) error {
	return StatusUpdate().
		WithStatus(status).
		Send(ctx, conn)
}

// StatusUpdateAsync sends a message to update the user's status.
func (conn *Conn) StatusUpdateAsync(ctx context.Context, status string, f func(error)) {
	StatusUpdate().
		WithStatus(status).
		Async(ctx, conn, f)
}

// OnConnect adds a connect callback.
func (conn *Conn) OnConnect(ctx context.Context, f func()) {
}

// OnDisconnect adds a disconnect callback.
func (conn *Conn) OnDisconnect(ctx context.Context, f func()) {
}

// OnError adds an error callback.
func (conn *Conn) OnError(ctx context.Context, f func(*ErrorMsg)) {
}

// OnChannelMessage adds a channel message callback.
func (conn *Conn) OnChannelMessage(ctx context.Context, f func(*ChannelMessageMsg)) {
}

// OnChannelPresence adds a channel presence callback.
func (conn *Conn) OnChannelPresenceEvent(ctx context.Context, f func(*ChannelPresenceEventMsg)) {
}

// OnMatchPresence adds a match presence callback.
func (conn *Conn) OnMatchPresenceEvent(ctx context.Context, f func(*MatchPresenceEventMsg)) {
}

// OnNotifications adds a notifications callback.
func (conn *Conn) OnNotifications(ctx context.Context, f func(*NotificationsMsg)) {
}

// OnStatusPresence adds a status presence callback.
func (conn *Conn) OnStatusPresenceEvent(ctx context.Context, f func(*StatusPresenceEventMsg)) {
}

// OnStreamPresence adds a stream presence callback.
func (conn *Conn) OnStreamPresenceEvent(ctx context.Context, f func(*StreamPresenceEventMsg)) {
}

// OnStreamData adds a stream data callback.
func (conn *Conn) OnStreamData(ctx context.Context, f func(*StreamDataMsg)) {
}

// req wraps a request and results.
type req struct {
	msg EnvelopeBuilder
	v   EnvelopeBuilder
	err chan error
}

// RealtimeError wraps a nakama realtime websocket error.
type RealtimeError struct {
	Code    rtapi.Error_Code
	Message string
	Context map[string]string
}

// NewRealtimeError creates a nakama realtime websocket error from an error
// message.
func NewRealtimeError(err *rtapi.Error) error {
	return &RealtimeError{
		Code:    rtapi.Error_Code(err.Code),
		Message: err.Message,
		Context: err.Context,
	}
}

// Error satisfies the error interface.
func (err *RealtimeError) Error() string {
	var s []string
	keys := maps.Keys(err.Context)
	sort.Strings(keys)
	for _, k := range keys {
		s = append(s, k+":"+err.Context[k])
	}
	var extra string
	if len(s) != 0 {
		extra = " <" + strings.Join(s, " ") + ">"
	}
	return fmt.Sprintf("realtime socket error %s (%d): %s%s", err.Code, err.Code, err.Message, extra)
}

// ConnOption is a nakama realtime websocket connection option.
type ConnOption func(*Conn)

// WithConnHandler is a nakama websocket connection option to set the Handler
// used.
func WithConnHandler(h Handler) ConnOption {
	return func(conn *Conn) {
		conn.h = h
	}
}

// WithConnUrl is a nakama websocket connection option to set the websocket
// URL.
func WithConnUrl(urlstr string) ConnOption {
	return func(conn *Conn) {
		conn.url = urlstr
	}
}

// WithConnToken is a nakama websocket connection option to set the auth token
// for the websocket.
func WithConnToken(token string) ConnOption {
	return func(conn *Conn) {
		conn.token = token
	}
}

// WithConnFormat is a nakama websocket connection option to set the message
// encoding format (either "json" or "protobuf").
func WithConnFormat(format string) ConnOption {
	return func(conn *Conn) {
		switch s := strings.ToLower(format); s {
		case "protobuf":
		case "json":
			conn.binary = false
		default:
			panic(fmt.Sprintf("invalid websocket format %q", format))
		}
	}
}

// WithConnQuery is a nakama websocket connection option to add an additional
// key/value query param on the websocket URL.
//
// Note: this should not be used to set "token" or "format". Use WithConnToken
// and WithConnFormat, respectively, to change the token and format query
// params.
func WithConnQuery(key, value string) ConnOption {
	return func(conn *Conn) {
		conn.query.Set(key, value)
	}
}

// WithConnLang is a nakama websocket connection option to set the lang query
// param on the websocket URL.
func WithConnLang(lang string) ConnOption {
	return func(conn *Conn) {
		conn.query.Set("lang", lang)
	}
}

// WithConnCreateStatus is a nakama websocket connection option to set the
// status query param on the websocket URL.
func WithConnCreateStatus(status bool) ConnOption {
	return func(conn *Conn) {
		conn.query.Set("status", strconv.FormatBool(status))
	}
}
