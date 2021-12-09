package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/gjson"

	"github.com/matrix-org/complement/internal/b"
)

// RequestOpt is a functional option which will modify an outgoing HTTP request.
// See functions starting with `With...` in this package for more info.
type RequestOpt func(req *http.Request)

// SyncCheckOpt is a functional option for use with SyncUntil which should return <nil> if
// the response satisfies the check, else return a human friendly error.
// The result object is the entire /sync response from this request.
type SyncCheckOpt func(clientUserID string, topLevelSyncJSON gjson.Result) error

// SyncReq contains all the /sync request configuration options. The empty struct `SyncReq{}` is valid
// which will do a full /sync due to lack of a since token.
type SyncReq struct {
	// A point in time to continue a sync from. This should be the next_batch token returned by an
	// earlier call to this endpoint.
	Since string
	// The ID of a filter created using the filter API or a filter JSON object encoded as a string.
	// The server will detect whether it is an ID or a JSON object by whether the first character is
	// a "{" open brace. Passing the JSON inline is best suited to one off requests. Creating a
	// filter using the filter API is recommended for clients that reuse the same filter multiple
	// times, for example in long poll requests.
	Filter string
	// Controls whether to include the full state for all rooms the user is a member of.
	// If this is set to true, then all state events will be returned, even if since is non-empty.
	// The timeline will still be limited by the since parameter. In this case, the timeout parameter
	// will be ignored and the query will return immediately, possibly with an empty timeline.
	// If false, and since is non-empty, only state which has changed since the point indicated by
	// since will be returned.
	// By default, this is false.
	FullState bool
	// Controls whether the client is automatically marked as online by polling this API. If this
	// parameter is omitted then the client is automatically marked as online when it uses this API.
	// Otherwise if the parameter is set to “offline” then the client is not marked as being online
	// when it uses this API. When set to “unavailable”, the client is marked as being idle.
	// One of: [offline online unavailable].
	SetPresence string
	// The maximum time to wait, in milliseconds, before returning this request. If no events
	// (or other data) become available before this time elapses, the server will return a response
	// with empty fields.
	// By default, this is 1000 for Complement testing.
	TimeoutMillis string // string for easier conversion to query params
}

type CSAPI struct {
	UserID      string
	AccessToken string
	BaseURL     string
	Client      *http.Client
	// how long are we willing to wait for SyncUntil.... calls
	SyncUntilTimeout time.Duration
	// True to enable verbose logging
	Debug bool

	txnID int
}

// UploadContent uploads the provided content with an optional file name. Fails the test on error. Returns the MXC URI.
func (c *CSAPI) UploadContent(t *testing.T, fileBody []byte, fileName string, contentType string) string {
	t.Helper()
	query := url.Values{}
	if fileName != "" {
		query.Set("filename", fileName)
	}
	res := c.MustDoFunc(
		t, "POST", []string{"_matrix", "media", "r0", "upload"},
		WithRawBody(fileBody), WithContentType(contentType), WithQueries(query),
	)
	body := ParseJSON(t, res)
	return GetJSONFieldStr(t, body, "content_uri")
}

// DownloadContent downloads media from the server, returning the raw bytes and the Content-Type. Fails the test on error.
func (c *CSAPI) DownloadContent(t *testing.T, mxcUri string) ([]byte, string) {
	t.Helper()
	mxcParts := strings.Split(strings.TrimPrefix(mxcUri, "mxc://"), "/")
	origin := mxcParts[0]
	mediaId := strings.Join(mxcParts[1:], "/")
	res := c.MustDo(t, "GET", []string{"_matrix", "media", "r0", "download", origin, mediaId}, struct{}{})
	contentType := res.Header.Get("Content-Type")
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error(err)
	}
	return b, contentType
}

// CreateRoom creates a room with an optional HTTP request body. Fails the test on error. Returns the room ID.
func (c *CSAPI) CreateRoom(t *testing.T, creationContent interface{}) string {
	t.Helper()
	res := c.MustDo(t, "POST", []string{"_matrix", "client", "r0", "createRoom"}, creationContent)
	body := ParseJSON(t, res)
	return GetJSONFieldStr(t, body, "room_id")
}

// JoinRoom joins the room ID or alias given, else fails the test. Returns the room ID.
func (c *CSAPI) JoinRoom(t *testing.T, roomIDOrAlias string, serverNames []string) string {
	t.Helper()
	// construct URL query parameters
	query := make(url.Values, len(serverNames))
	for _, serverName := range serverNames {
		query.Add("server_name", serverName)
	}
	// join the room
	res := c.MustDoFunc(t, "POST", []string{"_matrix", "client", "r0", "join", roomIDOrAlias}, WithQueries(query))
	// return the room ID if we joined with it
	if roomIDOrAlias[0] == '!' {
		return roomIDOrAlias
	}
	// otherwise we should be told the room ID if we joined via an alias
	body := ParseJSON(t, res)
	return GetJSONFieldStr(t, body, "room_id")
}

// LeaveRoom joins the room ID, else fails the test.
func (c *CSAPI) LeaveRoom(t *testing.T, roomID string) {
	t.Helper()
	// leave the room
	c.MustDoFunc(t, "POST", []string{"_matrix", "client", "r0", "rooms", roomID, "leave"})
}

// InviteRoom invites userID to the room ID, else fails the test.
func (c *CSAPI) InviteRoom(t *testing.T, roomID string, userID string) {
	t.Helper()
	// Invite the user to the room
	body := map[string]interface{}{
		"user_id": userID,
	}
	c.MustDo(t, "POST", []string{"_matrix", "client", "r0", "rooms", roomID, "invite"}, body)
}

// SendEventSynced sends `e` into the room and waits for its event ID to come down /sync.
// Returns the event ID of the sent event.
func (c *CSAPI) SendEventSynced(t *testing.T, roomID string, e b.Event) string {
	t.Helper()
	c.txnID++
	paths := []string{"_matrix", "client", "r0", "rooms", roomID, "send", e.Type, strconv.Itoa(c.txnID)}
	if e.StateKey != nil {
		paths = []string{"_matrix", "client", "r0", "rooms", roomID, "state", e.Type, *e.StateKey}
	}
	res := c.MustDo(t, "PUT", paths, e.Content)
	body := ParseJSON(t, res)
	eventID := GetJSONFieldStr(t, body, "event_id")
	t.Logf("SendEventSynced waiting for event ID %s", eventID)
	c.MustSyncUntil(t, SyncReq{}, SyncTimelineHas(roomID, func(r gjson.Result) bool {
		return r.Get("event_id").Str == eventID
	}))
	return eventID
}

// Perform a single /sync request with the given request options. To sync until something happens,
// see `SyncUntil`.
//
// Fails the test if the /sync request does not return 200 OK.
// Returns the top-level parsed /sync response JSON as well as the next_batch token from the response.
func (c *CSAPI) MustSync(t *testing.T, syncReq SyncReq) (gjson.Result, string) {
	t.Helper()
	query := url.Values{
		"timeout": []string{"1000"},
	}
	// configure the HTTP request based on SyncReq
	if syncReq.TimeoutMillis != "" {
		query["timeout"] = []string{syncReq.TimeoutMillis}
	}
	if syncReq.Since != "" {
		query["since"] = []string{syncReq.Since}
	}
	if syncReq.Filter != "" {
		query["filter"] = []string{syncReq.Filter}
	}
	if syncReq.FullState {
		query["full_state"] = []string{"true"}
	}
	if syncReq.SetPresence != "" {
		query["set_presence"] = []string{syncReq.SetPresence}
	}
	res := c.MustDoFunc(t, "GET", []string{"_matrix", "client", "r0", "sync"}, WithQueries(query))
	body := ParseJSON(t, res)
	result := gjson.ParseBytes(body)
	nextBatch := GetJSONFieldStr(t, body, "next_batch")
	return result, nextBatch
}

// MustSyncUntil blocks and continually calls /sync (advancing the since token) until all the
// check functions return no error. Returns the final/latest since token.
//
// Initial /sync example: (no since token)
//   bob.InviteRoom(t, roomID, alice.UserID)
//   alice.JoinRoom(t, roomID, nil)
//   alice.MustSyncUntil(t, client.SyncReq{}, client.SyncJoinedTo(alice.UserID, roomID))
//
// Incremental /sync example: (test controls since token)
//    since := alice.MustSyncUntil(t, client.SyncReq{TimeoutMillis: "0"}) // get a since token
//    bob.InviteRoom(t, roomID, alice.UserID)
//    since = alice.MustSyncUntil(t, client.SyncReq{Since: since}, client.SyncInvitedTo(alice.UserID, roomID))
//    alice.JoinRoom(t, roomID, nil)
//    alice.MustSyncUntil(t, client.SyncReq{Since: since}, client.SyncJoinedTo(alice.UserID, roomID))
//
// Checking multiple parts of /sync:
//    alice.MustSyncUntil(
//        t, client.SyncReq{},
//        client.SyncJoinedTo(alice.UserID, roomID),
//        client.SyncJoinedTo(alice.UserID, roomID2),
//        client.SyncJoinedTo(alice.UserID, roomID3),
//    )
//
// Check functions are unordered and independent. Once a check function returns true it is removed
// from the list of checks and won't be called again.
//
// In the unlikely event that you want all the checkers to pass *explicitly* in a single /sync
// response (e.g to assert some form of atomic update which updates multiple parts of the /sync
// response at once) then make your own checker function which does this.
//
// In the unlikely event that you need ordering on your checks, call MustSyncUntil multiple times
// with a single checker, and reuse the returned since token, as in the "Incremental sync" example.
//
// Will time out after CSAPI.SyncUntilTimeout. Returns the latest since token used.
func (c *CSAPI) MustSyncUntil(t *testing.T, syncReq SyncReq, checks ...SyncCheckOpt) string {
	t.Helper()
	start := time.Now()
	numResponsesReturned := 0
	checkers := make([]struct {
		check SyncCheckOpt
		errs  []string
	}, len(checks))
	for i := range checks {
		c := checkers[i]
		c.check = checks[i]
		checkers[i] = c
	}
	printErrors := func() string {
		err := "Checkers:\n"
		for _, c := range checkers {
			err += strings.Join(c.errs, "\n")
			err += ", "
		}
		return err
	}
	for {
		if time.Since(start) > c.SyncUntilTimeout {
			t.Fatalf("%s MustSyncUntil: timed out after %v. Seen %d /sync responses. %s", c.UserID, time.Since(start), numResponsesReturned, printErrors())
		}
		response, nextBatch := c.MustSync(t, syncReq)
		syncReq.Since = nextBatch
		numResponsesReturned += 1

		for i := 0; i < len(checkers); i++ {
			err := checkers[i].check(c.UserID, response)
			if err == nil {
				// check passed, removed from checkers
				checkers = append(checkers[:i], checkers[i+1:]...)
				i--
			} else {
				c := checkers[i]
				c.errs = append(c.errs, fmt.Sprintf("[t=%v] Response #%d: %s", time.Since(start), numResponsesReturned, err))
				checkers[i] = c
			}
		}
		if len(checkers) == 0 {
			// every checker has passed!
			return syncReq.Since
		}
	}
}

// SyncUntil blocks and continually calls /sync until
// - the response contains a particular `key`, and
// - its corresponding value is an array
// - some element in that array makes the `check` function return true.
// If the `check` function fails the test, the failing event will be automatically logged.
// Will time out after CSAPI.SyncUntilTimeout.
//
// Returns the `next_batch` token from the last /sync response. This can be passed as
// `since` to sync from this point forward only.
func (c *CSAPI) SyncUntil(t *testing.T, since, filter, key string, check func(gjson.Result) bool) string {
	t.Helper()
	start := time.Now()
	checkCounter := 0
	// Print failing events in a defer() so we handle t.Fatalf in the same way as t.Errorf
	var wasFailed = t.Failed()
	var lastEvent *gjson.Result
	timedOut := false
	defer func() {
		if !wasFailed && t.Failed() {
			raw := ""
			if lastEvent != nil {
				raw = lastEvent.Raw
			}
			if !timedOut {
				t.Logf("SyncUntil: failing event %s", raw)
			}
		}
	}()
	for {
		if time.Since(start) > c.SyncUntilTimeout {
			timedOut = true
			t.Fatalf("SyncUntil: timed out. Called check function %d times", checkCounter)
		}
		query := url.Values{
			"timeout": []string{"1000"},
		}
		if since != "" {
			query["since"] = []string{since}
		}
		if filter != "" {
			query["filter"] = []string{filter}
		}
		res := c.MustDoFunc(t, "GET", []string{"_matrix", "client", "r0", "sync"}, WithQueries(query))
		body := ParseJSON(t, res)
		since = GetJSONFieldStr(t, body, "next_batch")
		keyRes := gjson.GetBytes(body, key)
		if keyRes.IsArray() {
			events := keyRes.Array()
			for i, ev := range events {
				lastEvent = &events[i]
				if check(ev) {
					return since
				}
				wasFailed = t.Failed()
				checkCounter++
			}
		}
	}
}

//RegisterUser will register the user with given parameters and
// return user ID & access token, and fail the test on network error
func (c *CSAPI) RegisterUser(t *testing.T, localpart, password string) (userID, accessToken string) {
	t.Helper()
	reqBody := map[string]interface{}{
		"auth": map[string]string{
			"type": "m.login.dummy",
		},
		"username": localpart,
		"password": password,
	}
	res := c.MustDo(t, "POST", []string{"_matrix", "client", "r0", "register"}, reqBody)

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("unable to read response body: %v", err)
	}

	userID = gjson.GetBytes(body, "user_id").Str
	accessToken = gjson.GetBytes(body, "access_token").Str
	return userID, accessToken
}

// GetCapbabilities queries the server's capabilities
func (c *CSAPI) GetCapabilities(t *testing.T) []byte {
	t.Helper()
	res := c.MustDoFunc(t, "GET", []string{"_matrix", "client", "r0", "capabilities"})
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("unable to read response body: %v", err)
	}
	return body
}

// GetDefaultRoomVersion returns the server's default room version
func (c *CSAPI) GetDefaultRoomVersion(t *testing.T) gomatrixserverlib.RoomVersion {
	t.Helper()
	capabilities := c.GetCapabilities(t)
	defaultVersion := gjson.GetBytes(capabilities, `capabilities.m\.room_versions.default`)
	if !defaultVersion.Exists() {
		// spec says use RoomV1 in this case
		return gomatrixserverlib.RoomVersionV1
	}

	return gomatrixserverlib.RoomVersion(defaultVersion.Str)
}

// MustDo will do the HTTP request and fail the test if the response is not 2xx
//
// Deprecated: Prefer MustDoFunc. MustDo is the older format which doesn't allow for vargs
// and will be removed in the future. MustDoFunc also logs HTTP response bodies on error.
func (c *CSAPI) MustDo(t *testing.T, method string, paths []string, jsonBody interface{}) *http.Response {
	t.Helper()
	res := c.DoFunc(t, method, paths, WithJSONBody(t, jsonBody))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		t.Fatalf("CSAPI.MustDo %s %s returned HTTP %d", method, res.Request.URL.String(), res.StatusCode)
	}
	return res
}

// WithRawBody sets the HTTP request body to `body`
func WithRawBody(body []byte) RequestOpt {
	return func(req *http.Request) {
		req.Body = ioutil.NopCloser(bytes.NewBuffer(body))
		// we need to manually set this because we don't set the body
		// in http.NewRequest due to using functional options, and only in NewRequest
		// does the stdlib set this for us.
		req.ContentLength = int64(len(body))
	}
}

// WithContentType sets the HTTP request Content-Type header to `cType`
func WithContentType(cType string) RequestOpt {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", cType)
	}
}

// WithJSONBody sets the HTTP request body to the JSON serialised form of `obj`
func WithJSONBody(t *testing.T, obj interface{}) RequestOpt {
	return func(req *http.Request) {
		t.Helper()
		b, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("CSAPI.Do failed to marshal JSON body: %s", err)
		}
		WithRawBody(b)(req)
	}
}

// WithQueries sets the query parameters on the request.
// This function should not be used to set an "access_token" parameter for Matrix authentication.
// Instead, set CSAPI.AccessToken.
func WithQueries(q url.Values) RequestOpt {
	return func(req *http.Request) {
		req.URL.RawQuery = q.Encode()
	}
}

// MustDoFunc is the same as DoFunc but fails the test if the returned HTTP response code is not 2xx.
func (c *CSAPI) MustDoFunc(t *testing.T, method string, paths []string, opts ...RequestOpt) *http.Response {
	t.Helper()
	res := c.DoFunc(t, method, paths, opts...)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		body, _ := ioutil.ReadAll(res.Body)
		t.Fatalf("CSAPI.MustDoFunc response return non-2xx code: %s - body: %s", res.Status, string(body))
	}
	return res
}

// DoFunc performs an arbitrary HTTP request to the server. This function supports RequestOpts to set
// extra information on the request such as an HTTP request body, query parameters and content-type.
// See all functions in this package starting with `With...`.
//
// Fails the test if an HTTP request could not be made or if there was a network error talking to the
// server. To do assertions on the HTTP response, see the `must` package. For example:
//    must.MatchResponse(t, res, match.HTTPResponse{
//    	StatusCode: 400,
//    	JSON: []match.JSON{
//    		match.JSONKeyEqual("errcode", "M_INVALID_USERNAME"),
//    	},
//    })
func (c *CSAPI) DoFunc(t *testing.T, method string, paths []string, opts ...RequestOpt) *http.Response {
	t.Helper()
	for i := range paths {
		paths[i] = url.PathEscape(paths[i])
	}
	reqURL := c.BaseURL + "/" + strings.Join(paths, "/")
	req, err := http.NewRequest(method, reqURL, nil)
	if err != nil {
		t.Fatalf("CSAPI.DoFunc failed to create http.NewRequest: %s", err)
	}
	// set defaults before RequestOpts
	if c.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}

	// set functional options
	for _, o := range opts {
		o(req)
	}
	// set defaults after RequestOpts
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// debug log the request
	if c.Debug {
		t.Logf("Making %s request to %s", method, reqURL)
		contentType := req.Header.Get("Content-Type")
		if contentType == "application/json" || strings.HasPrefix(contentType, "text/") {
			if req.Body != nil {
				body, _ := ioutil.ReadAll(req.Body)
				t.Logf("Request body: %s", string(body))
				req.Body = ioutil.NopCloser(bytes.NewBuffer(body))
			}
		} else {
			t.Logf("Request body: <binary:%s>", contentType)
		}
	}
	// Perform the HTTP request
	res, err := c.Client.Do(req)
	if err != nil {
		t.Fatalf("CSAPI.DoFunc response returned error: %s", err)
	}
	// debug log the response
	if c.Debug && res != nil {
		var dump []byte
		dump, err = httputil.DumpResponse(res, true)
		if err != nil {
			t.Fatalf("CSAPI.DoFunc failed to dump response body: %s", err)
		}
		t.Logf("%s", string(dump))
	}
	return res
}

// NewLoggedClient returns an http.Client which logs requests/responses
func NewLoggedClient(t *testing.T, hsName string, cli *http.Client) *http.Client {
	t.Helper()
	if cli == nil {
		cli = &http.Client{
			Timeout: 30 * time.Second,
		}
	}
	transport := cli.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	cli.Transport = &loggedRoundTripper{t, hsName, transport}
	return cli
}

type loggedRoundTripper struct {
	t      *testing.T
	hsName string
	wrap   http.RoundTripper
}

func (t *loggedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	res, err := t.wrap.RoundTrip(req)
	if err != nil {
		t.t.Logf("%s %s%s => error: %s (%s)", req.Method, t.hsName, req.URL.Path, err, time.Since(start))
	} else {
		t.t.Logf("%s %s%s => %s (%s)", req.Method, t.hsName, req.URL.Path, res.Status, time.Since(start))
	}
	return res, err
}

// GetJSONFieldStr extracts a value from a byte-encoded JSON body given a search key
func GetJSONFieldStr(t *testing.T, body []byte, wantKey string) string {
	t.Helper()
	res := gjson.GetBytes(body, wantKey)
	if !res.Exists() {
		t.Fatalf("JSONFieldStr: key '%s' missing from %s", wantKey, string(body))
	}
	if res.Str == "" {
		t.Fatalf("JSONFieldStr: key '%s' is not a string, body: %s", wantKey, string(body))
	}
	return res.Str
}

func GetJSONFieldStringArray(t *testing.T, body []byte, wantKey string) []string {
	t.Helper()

	res := gjson.GetBytes(body, wantKey)

	if !res.Exists() {
		t.Fatalf("JSONFieldStr: key '%s' missing from %s", wantKey, string(body))
	}

	arrLength := len(res.Array())
	arr := make([]string, arrLength)
	i := 0
	res.ForEach(func(key, value gjson.Result) bool {
		arr[i] = value.Str

		// Keep iterating
		i++
		return true
	})

	return arr
}

// ParseJSON parses a JSON-encoded HTTP Response body into a byte slice
func ParseJSON(t *testing.T, res *http.Response) []byte {
	t.Helper()
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("MustParseJSON: reading HTTP response body returned %s", err)
	}
	if !gjson.ValidBytes(body) {
		t.Fatalf("MustParseJSON: Response is not valid JSON")
	}
	return body
}

// GjsonEscape escapes . and * from the input so it can be used with gjson.Get
func GjsonEscape(in string) string {
	in = strings.ReplaceAll(in, ".", `\.`)
	in = strings.ReplaceAll(in, "*", `\*`)
	return in
}

// Check that the timeline for `roomID` has an event which passes the check function.
func SyncTimelineHas(roomID string, check func(gjson.Result) bool) SyncCheckOpt {
	return func(clientUserID string, topLevelSyncJSON gjson.Result) error {
		err := loopArray(
			topLevelSyncJSON, "rooms.join."+GjsonEscape(roomID)+".timeline.events", check,
		)
		if err == nil {
			return nil
		}
		return fmt.Errorf("SyncTimelineHas(%s): %s", roomID, err)
	}
}

// Checks that `userID` gets invited to `roomID`.
//
// This checks different parts of the /sync response depending on the client making the request.
// If the client is also the person being invited to the room then the 'invite' block will be inspected.
// If the client is different to the person being invited then the 'join' block will be inspected.
func SyncInvitedTo(userID, roomID string) SyncCheckOpt {
	return func(clientUserID string, topLevelSyncJSON gjson.Result) error {
		// two forms which depend on what the client user is:
		// - passively viewing an invite for a room you're joined to (timeline events)
		// - actively being invited to a room.
		if clientUserID == userID {
			// active
			err := loopArray(
				topLevelSyncJSON, "rooms.invite."+GjsonEscape(roomID)+".invite_state.events",
				func(ev gjson.Result) bool {
					return ev.Get("type").Str == "m.room.member" && ev.Get("state_key").Str == userID && ev.Get("content.membership").Str == "invite"
				},
			)
			if err != nil {
				return fmt.Errorf("SyncInvitedTo(%s): %s", roomID, err)
			}
			return nil
		}
		// passive
		return SyncTimelineHas(roomID, func(ev gjson.Result) bool {
			return ev.Get("type").Str == "m.room.member" && ev.Get("state_key").Str == userID && ev.Get("content.membership").Str == "invite"
		})(clientUserID, topLevelSyncJSON)
	}
}

// Check that `userID` gets joined to `roomID` by inspecting the join timeline for a membership event.
func SyncJoinedTo(userID, roomID string) SyncCheckOpt {
	return func(clientUserID string, topLevelSyncJSON gjson.Result) error {
		// awkward wrapping to get the error message correct at the start :/
		err := SyncTimelineHas(roomID, func(ev gjson.Result) bool {
			return ev.Get("type").Str == "m.room.member" && ev.Get("state_key").Str == userID && ev.Get("content.membership").Str == "join"
		})(clientUserID, topLevelSyncJSON)
		if err == nil {
			return nil
		}
		return fmt.Errorf("SyncJoinedTo(%s,%s): %s", userID, roomID, err)
	}
}

// Calls the `check` function for each global account data event, and returns with success if the
// check function returns true.
func SyncGlobalAccountDataHas(check func(gjson.Result) bool) SyncCheckOpt {
	return func(clientUserID string, topLevelSyncJSON gjson.Result) error {
		return loopArray(topLevelSyncJSON, "account_data.events", check)
	}
}

func loopArray(object gjson.Result, key string, check func(gjson.Result) bool) error {
	array := object.Get(key)
	if !array.Exists() {
		return fmt.Errorf("Key %s does not exist", key)
	}
	if !array.IsArray() {
		return fmt.Errorf("Key %s exists but it isn't an array", key)
	}
	goArray := array.Array()
	for _, ev := range goArray {
		if check(ev) {
			return nil
		}
	}
	return fmt.Errorf("check function did not pass for %d elements: %v", len(goArray), array.Raw)
}
