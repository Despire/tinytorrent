package status

import (
	"bytes"
	"cmp"
	"crypto/sha1"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"time"

	"github.com/Despire/tinytorrent/cmd/cli/client/internal/tracker"
	"github.com/Despire/tinytorrent/p2p/messagesv1"
	"github.com/Despire/tinytorrent/p2p/peer"
)

func (t *Tracker) CancelDownload()                      { close(t.download.cancel); t.download.wg.Wait() }
func (t *Tracker) WaitUntilDownloaded() <-chan struct{} { return t.download.completed }

func (t *Tracker) UpdateSeeders(resp *tracker.Response) error {
	if t.Downloaded.Load() == t.Torrent.BytesToDownload() {
		return nil
	}

	var errAll error

	for _, r := range resp.Peers {
		addr := net.JoinHostPort(r.IP, fmt.Sprint(r.Port))
		t.logger.Debug("initiating connection to peer", slog.String("addr", addr))
		if _, ok := t.peers.seeders.Load(addr); ok {
			continue
		}

		np := peer.NewSeeder(t.logger, r.PeerID, addr, t.Torrent.NumPieces())
		t.peers.seeders.Store(addr, np)
		t.download.wg.Add(1)
		go t.keepAliveSeeders(np)
	}

	return errAll
}

func (t *Tracker) downloadScheduler() {
	// pool in random order
	// TODO: change to rarest piece.
	unverified := t.BitField.MissingPieces()
	rand.Shuffle(len(unverified), func(i, j int) {
		unverified[i], unverified[j] = unverified[j], unverified[i]
	})

	currentRate := int64(0)
	rateTicker := time.NewTicker(rateTick)
	for {
		select {
		case <-t.stop:
			t.logger.Info("shutting down piece downloader, closed tracker")
			t.download.wg.Done()
			return
		case <-t.download.cancel:
			t.logger.Info("shutting down piece downloader, canceled download")
			t.download.wg.Done()
			return
		case <-rateTicker.C:
			newRate := t.Downloaded.Load()
			diff := newRate - currentRate
			t.download.rate.Store(diff)
			currentRate = newRate
		default:
			freeSlots := 0
			for i := range t.download.requests {
				p := t.download.requests[i].Load()
				if p == nil {
					freeSlots++
					continue
				}

				p.l.Lock()

				// reschedule long running requests.
				for send := 0; send < len(p.InFlight); send++ {
					if req := p.InFlight[send]; !req.received && time.Since(req.send) > 15*time.Second {
						t.peers.seeders.Range(func(_, value any) bool {
							p := value.(*peer.Peer)
							canCancel := p.ConnectionStatus.Load() == uint32(peer.ConnectionEstablished)
							canCancel = canCancel && p.Status.Remote.Load() == uint32(peer.UnChoked)
							if canCancel {
								err := p.SendCancel(&messagesv1.Cancel{
									Index:  req.request.Index,
									Begin:  req.request.Begin,
									Length: req.request.Length,
								})
								if err != nil {
									t.logger.Error("failed to cancel request",
										slog.Any("err", err),
										slog.String("end_peer", p.Id),
										slog.String("req", fmt.Sprintf("%#v", req)),
									)
								}
							}
							return true
						})

						p.Pending = append(p.Pending, &messagesv1.Request{
							Index:  req.request.Index,
							Begin:  req.request.Begin,
							Length: req.request.Length,
						})
						p.InFlight[send] = nil
					}
				}
				p.InFlight = slices.DeleteFunc(p.InFlight, func(r *timedDownloadRequest) bool { return r == nil })

				// schedule pending requests to peers.
				for send := 0; send < len(p.Pending); send++ {
					piece := p.Pending[send]
					// select peer to contact for piece.
					var peers []*peer.Peer

					t.peers.seeders.Range(func(_, value any) bool {
						p := value.(*peer.Peer)
						canRequest := p.ConnectionStatus.Load() == uint32(peer.ConnectionEstablished)
						canRequest = canRequest && p.Status.Remote.Load() == uint32(peer.UnChoked)
						canRequest = canRequest && p.Bitfield.Check(piece.Index)
						if canRequest {
							peers = append(peers, p)
						}
						return true
					})

					if len(peers) == 0 {
						t.logger.Debug("no peers online that contain needed piece",
							slog.String("piece", fmt.Sprint(piece.Index)),
							slog.String("req", fmt.Sprintf("%#v", piece)),
						)
						continue
					}

					chosen := rand.IntN(len(peers))
					t.logger.Debug("sending request for piece",
						slog.String("end_peer", peers[chosen].Id),
						slog.String("req", fmt.Sprintf("%#v", piece)),
					)

					if err := peers[chosen].SendRequest(piece); err != nil {
						t.logger.Error("failed to issue request",
							slog.Any("err", err),
							slog.String("end_peer", peers[chosen].Id),
							slog.String("req", fmt.Sprintf("%#v", piece)),
						)
						continue
					}

					p.Pending[send] = nil
					p.InFlight = append(p.InFlight, &timedDownloadRequest{
						request: *piece,
						send:    time.Now(),
					})
				}
				p.Pending = slices.DeleteFunc(p.Pending, func(r *messagesv1.Request) bool { return r == nil })
				p.l.Unlock()
			}

			if len(unverified) == 0 { // we can't process any new pieces, wait for pending to finish.
				if freeSlots == len(t.download.requests) {
					t.logger.Info("Downloaded all pieces shutting down piece downloader")
					close(t.download.completed)
					t.download.wg.Done()
					return
				}
				continue
			}

			slot := -1
			for i := range t.download.requests {
				if t.download.requests[i].Load() == nil {
					slot = i
					break
				}
			}
			if slot < 0 {
				// no free slot
				continue
			}

			pieceStart := int64(unverified[0]) * t.Torrent.PieceLength
			pieceEnd := pieceStart + t.Torrent.PieceLength
			pieceEnd = min(pieceEnd, t.Torrent.BytesToDownload())
			pieceSize := pieceEnd - pieceStart

			pending := &pendingPiece{
				Index:      unverified[0],
				Downloaded: 0,
				Size:       pieceSize,
				Received:   nil,
				Pending:    nil,
				InFlight:   nil,
			}

			for p := int64(0); p < pieceSize; {
				nextBlockSize := int64(messagesv1.RequestSize)
				if pieceSize < p+nextBlockSize {
					nextBlockSize = pieceSize - p
				}

				pending.Pending = append(pending.Pending, &messagesv1.Request{
					Index:  pending.Index,
					Begin:  uint32(p),
					Length: uint32(nextBlockSize),
				})

				p += nextBlockSize
			}

			if !t.download.requests[slot].CompareAndSwap(nil, pending) {
				continue // slot was taken away.
			}

			unverified = unverified[1:]
		}
	}
}

func (t *Tracker) recvPieces(p *peer.Peer) {
	logger := t.logger.With(slog.String("peer_ip", p.Addr), slog.String("pid", p.Id))
	for {
		select {
		case recv, ok := <-p.SeederPieces():
			if !ok {
				logger.Debug("shutting piece downloader, channel closed")
				t.download.wg.Done()
				return
			}

			pieceIdx := -1
			var piece *pendingPiece
			for i := range t.download.requests {
				if r := t.download.requests[i].Load(); r != nil && r.Index == recv.Index {
					piece = r
					pieceIdx = i
					break
				}
			}
			if piece == nil {
				logger.Debug("received piece for untracked piece index", slog.String("piece_idx", fmt.Sprint(recv.Index)))
				continue
			}

			piece.l.Lock()

			req := slices.IndexFunc(piece.InFlight, func(r *timedDownloadRequest) bool {
				return r.request == messagesv1.Request{
					Index:  recv.Index,
					Begin:  recv.Begin,
					Length: uint32(len(recv.Block)),
				}
			})

			if req < 0 {
				logger.Debug("received piece for untracked piece",
					slog.String("piece_idx", fmt.Sprint(recv.Index)),
					slog.String("piece_offset", fmt.Sprint(recv.Begin)),
					slog.String("piece_length", fmt.Sprint(len(recv.Block))),
				)
				piece.l.Unlock()
				continue
			}

			// check for duplicates
			var skip bool
			for _, other := range piece.Received {
				duplicate := other.Begin == recv.Begin
				duplicate = duplicate && other.Index == recv.Index
				duplicate = duplicate && len(other.Block) == len(recv.Block)
				if duplicate {
					skip = true
					break
				}
			}
			if skip {
				piece.l.Unlock()
				continue
			}

			piece.Downloaded += int64(len(recv.Block))
			if piece.Downloaded > piece.Size {
				piece.l.Unlock()
				panic(fmt.Sprintf("recieved more data than expected for piece %v", recv.Index))
			}
			total := t.Downloaded.Add(int64(len(recv.Block)))

			piece.Received = append(piece.Received, recv)
			piece.InFlight[req].received = true // mark as received to it won't be rescheduled again.

			status := float64(piece.Downloaded) / float64(piece.Size)
			status *= 100
			logger.Debug("received piece",
				slog.String("piece", fmt.Sprint(recv.Index)),
				slog.String("downloaded_bytes", fmt.Sprint(piece.Downloaded)),
				slog.String("status", fmt.Sprintf("%.2f%%", status)),
			)

			if piece.Downloaded == piece.Size {
				slices.SortFunc(piece.Received, func(a, b *messagesv1.Piece) int { return cmp.Compare(a.Begin, b.Begin) })
				var data []byte
				for _, d := range piece.Received {
					data = append(data, d.Block...)
				}
				digest := sha1.Sum(data)

				// TODO: randomly choose to invalidate hashes to test stability of the implementation.
				// it should eventually still manage to download.
				if !bytes.Equal(digest[:], t.Torrent.PieceHash(recv.Index)) {
					logger.Debug("invalid piece sha1 hash, stop tracking", slog.String("piece", fmt.Sprint(recv.Index)))
					if err := p.Close(); err != nil {
						logger.Error("failed to close peer after invalid sha1 hash", slog.String("piece", fmt.Sprint(recv.Index)))
					}
					// retry downloading the piece again.
					if len(piece.Pending) != 0 {
						piece.l.Unlock()
						panic("malformed state, expected no pending requests when rescheduling piece for retry download")
					}
					for _, tr := range piece.InFlight {
						piece.Pending = append(piece.Pending, &messagesv1.Request{
							Index:  tr.request.Index,
							Begin:  tr.request.Begin,
							Length: tr.request.Length,
						})
					}
					piece.InFlight = nil
					piece.Received = nil
					piece.l.Unlock()
					continue
				}

				if err := t.Flush(recv.Index, data); err != nil {
					logger.Error("failed to flush piece", slog.Any("err", err), slog.String("piece", fmt.Sprint(recv.Index)))
					// retry downloading the piece again.
					if len(piece.Pending) != 0 {
						piece.l.Unlock()
						panic("malformed state, expected no pending requests when rescheduling piece for retry download")
					}
					for _, tr := range piece.InFlight {
						piece.Pending = append(piece.Pending, &messagesv1.Request{
							Index:  tr.request.Index,
							Begin:  tr.request.Begin,
							Length: tr.request.Length,
						})
					}
					piece.InFlight = nil
					piece.Received = nil
					piece.l.Unlock()
					continue
				}

				t.BitField.Set(recv.Index)

				logger.Debug("sending have message for verified piece", slog.String("piece", fmt.Sprint(recv.Index)))

				t.peers.seeders.Range(func(_, value any) bool {
					if p := value.(*peer.Peer); p.ConnectionStatus.Load() == uint32(peer.ConnectionEstablished) {
						if err := p.SendHave(&messagesv1.Have{Index: recv.Index}); err != nil {
							logger.Error("failed to send have piece, after verifying",
								slog.Any("err", err),
								slog.String("end_peer", p.Id),
								slog.String("piece", fmt.Sprint(recv.Index)),
							)
						}
					}
					return true
				})

				logger.Info("piece verified successfully",
					slog.String("status", fmt.Sprintf("%.2f%%", (float64(total)/float64(t.Torrent.BytesToDownload()))*100)),
					slog.String("kbps", fmt.Sprintf("%.2f", (float64(t.download.rate.Load())/1000.0)*100)),
					slog.String("piece", fmt.Sprint(recv.Index)),
				)

				// make place for a new piece to be scheduled.
				if !t.download.requests[pieceIdx].CompareAndSwap(piece, nil) {
					logger.Warn("two go-routines verified same piece", slog.String("piece", fmt.Sprint(recv.Index)))
				}
			}

			piece.l.Unlock()
		}
	}
}

func (t *Tracker) keepAliveSeeders(p *peer.Peer) {
	logger := t.logger.With(slog.String("peer_ip", p.Addr), slog.String("pid", p.Id))
	refresh := time.NewTicker(1 * time.Nanosecond) // first tick happens immediately.
	for {
		select {
		case <-t.stop:
			if err := p.SendNotInterested(); err != nil {
				logger.Error("failed to send not-interested msg, after successful download")
			}
			logger.Debug("shutting down peer refresher, stopped tracker")
			if err := p.Close(); err != nil {
				logger.Error("failed to close peer", slog.Any("err", err))
			}
			t.download.wg.Done()
			return
		case <-t.download.cancel:
			logger.Debug("shutting down peer refresher, canceled download")
			if err := p.Close(); err != nil {
				logger.Error("failed to close peer", slog.Any("err", err))
			}
			t.download.wg.Done()
			return
		case <-t.download.completed:
			if err := p.SendNotInterested(); err != nil {
				logger.Error("failed to send not-interested msg, after successful download")
			}
			logger.Debug("shutting down peer refresher, as torrent was downloaded")
			if err := p.Close(); err != nil {
				logger.Error("failed to close peer", slog.Any("err", err))
			}
			t.download.wg.Done()
			return
		case <-refresh.C:
			refresh.Reset(2 * time.Minute)
			switch s := peer.ConnectionStatus(p.ConnectionStatus.Load()); s {
			case peer.ConnectionPending, peer.ConnectionKilled:
				logger.Debug("attempting to connect with peer")
				if err := p.ConnectSeeder(); err != nil {
					logger.Error("failed to reconnect with peer", slog.Any("err", err))
					continue
				}
				logger.Debug("initiating handshake")
				if err := p.InitiateHandshakeV1(string(t.Torrent.Metadata.Hash[:]), t.clientID); err != nil {
					logger.Error("failed to initiating handshake", slog.Any("err", err))
					if err := p.Close(); err != nil {
						logger.Error("failed to close peer", slog.Any("err", err))
					}
					continue
				}
				if err := p.SendBitfield(t.BitField.Clone()); err != nil {
					logger.Error("failed to send bitfield msg")
				}
				if err := p.SendInterested(); err != nil {
					logger.Error("failed to send interested msg")
				}
				// Listen for pieces.
				t.download.wg.Add(1)
				go t.recvPieces(p)
			case peer.ConnectionEstablished:
				logger.Info("sending keep alive event on torrent peer")
				if err := p.SendKeepAlive(); err != nil {
					logger.Error("failed to keep alive", slog.Any("err", err))
				}
			}
		}
	}
}
