package server

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v2"
)

const (
	rtcpPLIInterval = time.Second * 3
)

type TrackEventType uint32

const (
	TrackEventTypeAdd = iota + 1
	TrackEventTypeRemove
)

type TrackEvent struct {
	ClientID string
	Track    *webrtc.Track
	Type     TrackEventType
}

type trackListener struct {
	log              Logger
	clientID         string
	peerConnection   *webrtc.PeerConnection
	localTracks      []*webrtc.Track
	localTracksMu    sync.RWMutex
	rtpSenderByTrack map[*webrtc.Track]*webrtc.RTPSender

	tracksChannel       chan TrackEvent
	tracksChannelClosed bool
	closeChannel        chan struct{}
	mu                  sync.RWMutex
	closeOnce           sync.Once
}

func newTrackListener(
	loggerFactory LoggerFactory,
	clientID string,
	peerConnection *webrtc.PeerConnection,
) *trackListener {
	p := &trackListener{
		log:              loggerFactory.GetLogger("peer"),
		clientID:         clientID,
		peerConnection:   peerConnection,
		rtpSenderByTrack: map[*webrtc.Track]*webrtc.RTPSender{},

		tracksChannel: make(chan TrackEvent),
		closeChannel:  make(chan struct{}),
	}

	p.log.Printf("[%s] Setting PeerConnection.OnTrack listener", clientID)
	peerConnection.OnTrack(p.handleTrack)

	return p
}

// FIXME add support for data channel messages for sending chat messages, and images/files

func (p *trackListener) Close() {
	p.closeOnce.Do(func() {
		close(p.closeChannel)

		p.mu.Lock()
		defer p.mu.Unlock()

		close(p.tracksChannel)
		p.tracksChannelClosed = true
	})
}

func (p *trackListener) TracksChannel() <-chan TrackEvent {
	return p.tracksChannel
}

func (p *trackListener) ClientID() string {
	return p.clientID
}

func (p *trackListener) AddTrack(track *webrtc.Track) error {
	p.localTracksMu.Lock()
	defer p.localTracksMu.Unlock()

	p.log.Printf("[%s] peer.AddTrack: add sendonly transceiver for track: %s", p.clientID, track.ID())
	rtpSender, err := p.peerConnection.AddTrack(track)
	// t, err := p.peerConnection.AddTransceiverFromTrack(
	// 	track,
	// 	webrtc.RtpTransceiverInit{
	// 		Direction: webrtc.RTPTransceiverDirectionSendonly,
	// 	},
	// )

	if err != nil {
		return fmt.Errorf("[%s] peer.AddTrack: error adding track: %s: %s", p.clientID, track.ID(), err)
	}

	// p.rtpSenderByTrack[track] = t.Sender()
	p.rtpSenderByTrack[track] = rtpSender
	return nil
}

func (p *trackListener) RemoveTrack(track *webrtc.Track) error {
	p.localTracksMu.Lock()
	defer p.localTracksMu.Unlock()
	p.log.Printf("[%s] peer.RemoveTrack: %s", p.clientID, track.ID())
	rtpSender, ok := p.rtpSenderByTrack[track]
	if !ok {
		return fmt.Errorf("[%s] peer.RemoveTrack: cannot find sender for track: %s", p.clientID, track.ID())
	}
	delete(p.rtpSenderByTrack, track)
	return p.peerConnection.RemoveTrack(rtpSender)
}

func (p *trackListener) handleTrack(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
	p.log.Printf("[%s] peer.handleTrack (id: %s, label: %s, type: %s, ssrc: %d)",
		p.clientID, remoteTrack.ID(), remoteTrack.Label(), remoteTrack.Kind(), remoteTrack.SSRC())
	localTrack, err := p.startCopyingTrack(remoteTrack)
	if err != nil {
		p.log.Printf("Error copying remote track: %s", err)
		return
	}
	p.localTracksMu.Lock()
	p.localTracks = append(p.localTracks, localTrack)
	p.localTracksMu.Unlock()

	p.log.Printf("[%s] peer.handleTrack add track to list of local tracks: %s", p.clientID, localTrack.ID())
	p.tracksChannel <- TrackEvent{p.clientID, localTrack, TrackEventTypeAdd}
}

func (p *trackListener) sendTrackEvent(t TrackEvent) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ch := p.tracksChannel
	if p.tracksChannelClosed {
		ch = nil
	}

	select {
	case ch <- t:
		p.log.Printf("[%s] sendTrackEvent success", p.clientID)
	case <-p.closeChannel:
		p.log.Printf("[%s] sendTrackEvent channel closed", p.clientID)
	}
}

func (p *trackListener) Tracks() []*webrtc.Track {
	return p.localTracks
}

func (p *trackListener) startCopyingTrack(remoteTrack *webrtc.Track) (*webrtc.Track, error) {
	remoteTrackID := remoteTrack.ID()
	if remoteTrackID == "" {
		remoteTrackID = NewUUIDBase62()
	}
	// this is the media stream ID we add the p.clientID in the string to know
	// which user the video came from and the remoteTrack.Label() so we can
	// associate audio/video tracks from the same MediaStream
	remoteTrackLabel := remoteTrack.Label()
	if remoteTrackLabel == "" {
		remoteTrackLabel = NewUUIDBase62()
	}
	localTrackLabel := "sfu_" + p.clientID + "_" + remoteTrackLabel

	localTrackID := "sfu_" + remoteTrackID
	p.log.Printf("[%s] peer.startCopyingTrack: (id: %s, label: %s) to (id: %s, label: %s), ssrc: %d",
		p.clientID, remoteTrack.ID(), remoteTrack.Label(), localTrackID, localTrackLabel, remoteTrack.SSRC())

	ssrc := remoteTrack.SSRC()
	// Create a local track, all our SFU clients will be fed via this track
	localTrack, err := p.peerConnection.NewTrack(remoteTrack.PayloadType(), ssrc, localTrackID, localTrackLabel)
	if err != nil {
		err = fmt.Errorf("[%s] peer.startCopyingTrack: error creating new track, trackID: %s, error: %s", p.clientID, remoteTrack.ID(), err)
		return nil, err
	}

	// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
	// This can be less wasteful by processing incoming RTCP events, then we would emit a NACK/PLI when a viewer requests it

	ticker := time.NewTicker(rtcpPLIInterval)
	go func() {
		writeRTCP := func() {
			err := p.peerConnection.WriteRTCP(
				[]rtcp.Packet{
					&rtcp.PictureLossIndication{
						MediaSSRC: ssrc,
					},
				},
			)
			if err != nil {
				p.log.Printf("[%s] Error sending rtcp PLI for local track: %s: %s",
					p.clientID,
					localTrackID,
					err,
				)
			}
		}

		writeRTCP()
		for range ticker.C {
			writeRTCP()
		}
	}()

	go func() {
		defer ticker.Stop()
		defer func() {
			p.mu.RLock()
			if !p.tracksChannelClosed {
				p.tracksChannel <- TrackEvent{p.clientID, localTrack, TrackEventTypeRemove}
			}
			p.mu.RUnlock()
		}()
		rtpBuf := make([]byte, 1400)
		for {
			i, err := remoteTrack.Read(rtpBuf)
			if err != nil {
				p.log.Printf(
					"[%s] Error reading from remote track: %s: %s",
					p.clientID,
					remoteTrack.ID(),
					err,
				)
				return
			}

			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			if _, err = localTrack.Write(rtpBuf[:i]); err != nil && err != io.ErrClosedPipe {
				p.log.Printf(
					"[%s] Error writing to local track: %s: %s",
					p.clientID,
					localTrackID,
					err,
				)
				return
			}
		}
	}()

	return localTrack, nil
}
