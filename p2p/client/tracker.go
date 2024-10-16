package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"

	"github.com/Despire/tinytorrent/bencoding"
)

// TrackerEvent represents state of the client during communication
// with the tracker.
type TrackerEvent string

const (
	// StartedEvent must be included by the first request to the tracker.
	StartedEvent TrackerEvent = "started"
	// StoppedEvent must be included if the client is shutting down gracefully.
	StoppedEvent TrackerEvent = "stopped"
	// CompletedEvent must be included when the download completes.
	CompletedEvent TrackerEvent = "completed"
)

type TrackerRequestParams struct {
	// 20-byte SHA1 hash of the value of the info key from the Metainfo file.
	InfoHash string
	// 20-byte string used as a unique ID for the client, generated by the client
	PeerID string
	// The port number that the client is listening on (Optional).
	Port *int64
	// The total amount uploaded (since the client sent the 'started' event to the tracker). (Optional).
	Uploaded *int64
	// The total amount downloaded (since the client sent the 'started' event to the tracker) (Optional).
	Downloaded *int64
	// The number of bytes this client still has to download
	//(The number of bytes needed to download to be 100% complete) (Optional).
	Left *int64
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
	Event *TrackerEvent
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

func (p TrackerRequestParams) Validate() error {
	if p.InfoHash == "" {
		return errors.New("info_hash is required")
	}
	if p.PeerID == "" {
		return errors.New("peer_id is required")
	}
	if p.Port != nil && *p.Port == 0 {
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
		case StartedEvent, StoppedEvent, CompletedEvent:
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

func (p TrackerRequestParams) Encode() string {
	values := url.Values{
		"info_hash": {p.InfoHash},
		"peer_id":   {p.PeerID},
	}
	if p.Port != nil {
		values.Set("port", strconv.Itoa(int(*p.Port)))
	}
	if p.Uploaded != nil {
		values.Set("uploaded", strconv.Itoa(int(*p.Uploaded)))
	}
	if p.Downloaded != nil {
		values.Set("downloaded", strconv.Itoa(int(*p.Downloaded)))
	}
	if p.Left != nil {
		values.Set("left", strconv.Itoa(int(*p.Left)))
	}
	if p.Compact != nil {
		values.Set("compact", strconv.Itoa(int(*p.Compact)))
	}
	if p.NoPeerId != nil {
		values.Set("no_peer_id", strconv.Itoa(int(*p.NoPeerId)))
	}
	if p.Event != nil {
		values.Set("event", string(*p.Event))
	}
	if p.IP != nil {
		values.Set("ip", *p.IP)
	}
	if p.NumWant != nil {
		values.Set("num_want", strconv.Itoa(int(*p.NumWant)))
	}
	if p.Key != nil {
		values.Set("key", *p.Key)
	}
	if p.TrackerID != nil {
		values.Set("tracker_id", *p.TrackerID)
	}
	return values.Encode()
}

type TrackerResponse struct {
	// Indicating what went wrong. If present no other keys may be present.
	FailureReason *string
	// Similar to FailureReason but response is valid.
	WarningMessage *string
	// Interval in seconds that the client should wait between sending
	// regular requests to the tracker.
	Interval *int64
	// Minimum announce interval. If present clients must not reannounce more
	// frequently than this.
	MinInterval *int64
	// ID that the client should send back on its next announcements
	// to the tracker. If the value is absent and it was received
	// by a previous response from the tracker that same value should
	// be re-used and not discarded.
	TrackerID *string
	// Number of peers with entire file (seeders).
	Complete *int64
	// Number of peers participating in the file (leechers).
	Incomplete *int64
	// Peers for the file.
	Peers []struct {
		PeerID string
		IP     string
		Port   int64
	}
}

func DecodeTrackerResponse(src io.Reader, out *TrackerResponse) error {
	if out == nil {
		panic("no response to fill, pased <nil>")
	}

	resp, err := bencoding.Decode(src)
	if err != nil {
		return fmt.Errorf("failed to decode body: %w", err)
	}
	if typ := resp.Type(); typ != bencoding.DictionaryType {
		return fmt.Errorf("expected response to be of type dictionary but got %v", typ)
	}

	dict := resp.(*bencoding.Dictionary).Dict

	if fr := dict["failure reason"]; fr != nil {
		l, ok := fr.(*bencoding.ByteString)
		if !ok {
			return fmt.Errorf("expected failure_reason to be of type Bytestring but was %T: ", fr)
		}
		out.FailureReason = (*string)(l)
		return nil // no other fields will be set.
	}

	if wm := dict["warning message"]; wm != nil {
		l, ok := wm.(*bencoding.ByteString)
		if !ok {
			return fmt.Errorf("expected warning_message to be of type Bytestring but was %T: ", wm)
		}
		out.WarningMessage = (*string)(l)
	}

	if i := dict["interval"]; i != nil {
		l, ok := i.(*bencoding.Integer)
		if !ok {
			return fmt.Errorf("expected interval to be of type Integer but was %T: ", i)
		}
		out.Interval = (*int64)(l)
	}

	if mi := dict["min interval"]; mi != nil {
		l, ok := mi.(*bencoding.Integer)
		if !ok {
			return fmt.Errorf("expected min_interval to be of type Integer but was %T: ", mi)
		}
		out.MinInterval = (*int64)(l)
	}

	if ti := dict["tracker id"]; ti != nil {
		l, ok := ti.(*bencoding.ByteString)
		if !ok {
			return fmt.Errorf("expected tracker_id to be of type Bytestring but was %T: ", ti)
		}
		out.TrackerID = (*string)(l)
	}

	if c := dict["complete"]; c != nil {
		l, ok := c.(*bencoding.Integer)
		if !ok {
			return fmt.Errorf("expected complete to be of type Integer but was %T: ", c)
		}
		out.Complete = (*int64)(l)
	}
	if inc := dict["incomplete"]; inc != nil {
		l, ok := inc.(*bencoding.Integer)
		if !ok {
			return fmt.Errorf("expected incomplete to be of type Integer but was %T: ", inc)
		}
		out.Incomplete = (*int64)(l)
	}

	if peers := dict["peers"]; peers != nil {
		switch peers.Type() {
		case bencoding.ListType: // non-compact
			wide := peers.(*bencoding.List)
			for _, peer := range *wide {
				peer, ok := peer.(*bencoding.Dictionary)
				if !ok {
					return fmt.Errorf("expected peer to be of type Dictionary but was %T: ", peer)
				}

				var peerData struct {
					PeerID string
					IP     string
					Port   int64
				}

				if id := peer.Dict["peer id"]; id != nil {
					l, ok := id.(*bencoding.ByteString)
					if !ok {
						return fmt.Errorf("expected peer_id to be of type Bytestring but was %T: ", id)
					}
					peerData.PeerID = string(*l)
				}

				ip := peer.Dict["ip"]
				if ip == nil {
					return errors.New("no ip listed for peer, inside peers list")
				}
				l, ok := ip.(*bencoding.ByteString)
				if !ok {
					return fmt.Errorf("expected peer_ip to be of type Bytestring but was %T: ", ip)
				}

				port := peer.Dict["port"]
				if port == nil {
					return fmt.Errorf("no port listed for peer, inside peers list")
				}
				p, ok := port.(*bencoding.Integer)
				if !ok {
					return fmt.Errorf("expected peer_port to be of type Integer but was %T: ", port)
				}

				peerData.IP = string(*l)
				peerData.Port = int64(*p)

				out.Peers = append(out.Peers, peerData)
			}

		case bencoding.ByteStringType: // compact
			compact := []byte(*(*string)(peers.(*bencoding.ByteString)))
			if len(compact)%6 != 0 {
				return fmt.Errorf("expected length of compact to be a multiple of 6 but got %v", len(compact))
			}
			for i := 0; i < len(compact); i += 6 {
				peer := compact[i : i+6]

				var peerData struct {
					PeerID string
					IP     string
					Port   int64
				}

				peerData.IP = net.IP(peer[:4]).String()
				peerData.Port = int64(binary.BigEndian.Uint16(peer[4:]))

				out.Peers = append(out.Peers, peerData)

			}
		default:
			return fmt.Errorf("peers were nor dictionary or bytestring type, got %T", peers)
		}
	}

	return nil
}

func CreateTrackerRequest(ctx context.Context, params *TrackerRequestParams) (*TrackerResponse, error) {
	panic("implement me")
}
