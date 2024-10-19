package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

func SetOptional[T any](val T) *T { return &val }

type RequestParams struct {
	// 20-byte SHA1 hash of the value of the info key from the Metainfo file.
	InfoHash string
	// 20-byte string used as a unique ID for the client, generated by the client
	PeerID string
	// The port number that the client is listening on.
	Port int64
	// The total amount uploaded (since the client sent the 'started' event to the tracker).
	Uploaded int64
	// The total amount downloaded (since the client sent the 'started' event to the tracker).
	Downloaded int64
	// The number of bytes this client still has to download
	//(The number of bytes needed to download to be 100% complete).
	Left int64
	// Setting this to 1 indicates that the client accepts a compact response. Possible values [0|1] (Optional).
	// NOTE: some trackers only support compact responses (for saving bandwidth) and either refuse requests
	// without "compact=1" or simply send a compact response unless the request contains "compact=0"
	// (in which case they will refuse the request.)
	Compact *int64
	// Ignores peer id in peers dictionary in the response.
	// This option is ignored if compact is enabled (Optional).
	NoPeerId *int64
	// If not specified then it is expected that the request
	// is one performed at regular intervals (Optional).
	Event *Event
	// The true IP address of the client (Optional).
	// Useful if client sits behind a proxy.
	IP *string
	// Number of peers that the client would like to receive
	// from tracker (Optional).
	NumWant *int64
	// Additional identification not shared with other peers.
	// Useful when proving identity in case of IP change.
	Key *string
	// If a previous announcement contained a tracker id it should be
	// set in the subsequent requests (Optional).
	TrackerID *string
}

func (p RequestParams) Validate() error {
	if p.InfoHash == "" {
		return errors.New("info_hash is required")
	}
	if p.PeerID == "" {
		return errors.New("peer_id is required")
	}
	if p.Port == 0 {
		return errors.New("port specified but provided value 0")
	}
	if p.Compact != nil {
		if *p.Compact != 1 && *p.Compact != 0 {
			return fmt.Errorf("compact specified but invalid provided value %v", *p.Compact)
		}
	}
	if p.NoPeerId != nil {
		if *p.NoPeerId != 1 && *p.NoPeerId != 0 {
			return fmt.Errorf("no_peer_id specified but provided value %v", *p.NoPeerId)
		}
		if p.Compact != nil {
			return fmt.Errorf("cannot have both no_peer_id and compact specified")
		}
	}
	if p.Event != nil {
		switch *p.Event {
		case EventStarted, EventStopped, EventCompleted:
		default:
			return fmt.Errorf("unknown event %v", *p.Event)
		}
	}
	if p.IP != nil {
		if ip := net.ParseIP(*p.IP); ip == nil {
			return fmt.Errorf("invalid ip %v", *p.IP)
		}
	}
	if p.NumWant != nil {
		if *p.NumWant < 0 {
			return fmt.Errorf("num_want %v cannot be negative", *p.NumWant)
		}
	}
	return nil
}

func (p RequestParams) Encode() string {
	values := url.Values{
		"info_hash":  {p.InfoHash},
		"peer_id":    {p.PeerID},
		"port":       {strconv.FormatInt(p.Port, 10)},
		"uploaded":   {strconv.FormatInt(p.Uploaded, 10)},
		"downloaded": {strconv.FormatInt(p.Downloaded, 10)},
		"left":       {strconv.FormatInt(p.Left, 10)},
	}

	if p.Compact != nil {
		values.Set("compact", strconv.Itoa(int(*p.Compact)))
	}
	if p.NoPeerId != nil {
		values.Set("no_peer_id", strconv.Itoa(int(*p.NoPeerId)))
	}
	if p.Event != nil && *p.Event != "" {
		values.Set("event", string(*p.Event))
	}
	if p.IP != nil && *p.IP != "" {
		values.Set("ip", *p.IP)
	}
	if p.NumWant != nil {
		values.Set("numwant", strconv.Itoa(int(*p.NumWant)))
	}
	if p.Key != nil && *p.Key != "" {
		values.Set("key", *p.Key)
	}
	if p.TrackerID != nil && *p.TrackerID != "" {
		values.Set("trackerid", *p.TrackerID)
	}
	return values.Encode()
}

func CreateRequest(ctx context.Context, announce string, params *RequestParams) (*Response, error) {
	if err := params.Validate(); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s?%s", announce, params.Encode()),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request send to tracker %s returned status code: %v, body: %s", announce, resp.StatusCode, body)
	}

	var info Response
	if err := DecodeResponse(bytes.NewReader(body), &info); err != nil {
		return nil, fmt.Errorf("failed to decode tracker response: %w", err)
	}

	if info.FailureReason != nil {
		return nil, fmt.Errorf("request to tracker failed: %s", *info.FailureReason)
	}

	return &info, nil
}