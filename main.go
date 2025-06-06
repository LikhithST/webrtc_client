// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

// play-from-disk demonstrates how to send video and/or audio to your browser from files saved to disk.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	fastclock "github.com/likhith/fastclock"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	audioFileName   = "output.ogg"
	videoFileName   = "output.ivf"
	oggPageDuration = time.Millisecond * 20
	httpEndpoint    = "https://webrtc.hopto.org:8080/offer" // Replace with your HTTP endpoint URL
)

type DataChannelMessage struct {
	FrameID                int64  `json:"frameID"`
	MessageSentTimeClient2 int64  `json:"messageSentTime_client2,omitempty"`
	MessageSentTimeSfu2    int64  `json:"messageSentTime_sfu2,omitempty"`
	MessageSentTimeSfu1    int64  `json:"messageSentTime_sfu1,omitempty"`
	MessageSentTimeClient1 int64  `json:"messageSentTime_client1,omitempty"`
	JitterSFU2             int64  `json:"jitter_sfu2,omitempty"`
	JitterSFU1             int64  `json:"jitter_sfu1,omitempty"`
	LatencyEndToEnd        int64  `json:"latency_end_to_end,omitempty"`
	MessageSendRate        int64  `json:"message_send_rate,omitempty"`
	Payload                []byte `json:"payload"`
}

var stats = struct {
	FrameID                *prometheus.GaugeVec
	MessageSentTimeClient2 *prometheus.GaugeVec
	MessageSentTimeSfu2    *prometheus.GaugeVec
	MessageSentTimeSfu1    *prometheus.GaugeVec
	MessageSentTimeClient1 *prometheus.GaugeVec
	LatencyEndToEnd        *prometheus.GaugeVec
	LatencyClient2ToSfu2   *prometheus.GaugeVec
	LatencySfu2ToSfu1      *prometheus.GaugeVec
	LatencySfu1ToClient1   *prometheus.GaugeVec
}{
	FrameID: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_frame_id",
			Help: "The ID of the frame being processed",
		},
		[]string{"frame_id"},
	),
	MessageSentTimeClient2: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_message_sent_time_client2",
			Help: "The time when the message was sent from client 2",
		},
		[]string{"message_sent_time_client2"},
	),
	MessageSentTimeSfu2: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_message_sent_time_sfu2",
			Help: "The time when the message was sent from SFU 2",
		},
		[]string{"message_sent_time_sfu2"},
	),
	MessageSentTimeSfu1: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_message_sent_time_sfu1",
			Help: "The time when the message was sent from SFU 1",
		},
		[]string{"message_sent_time_sfu1"},
	),
	MessageSentTimeClient1: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_message_sent_time_client1",
			Help: "The time when the message was sent from client 1",
		},
		[]string{"message_sent_time_client1"},
	),
	LatencyEndToEnd: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_latency_end_to_end",
			Help: "End-to-end latency of the message",
		},
		[]string{"latency_end_to_end"},
	),
	LatencyClient2ToSfu2: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_latency_client2_to_sfu2",
			Help: "Latency from client 2 to SFU 2",
		},
		[]string{"latency_client2_to_sfu2"},
	),
	LatencySfu2ToSfu1: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_latency_sfu2_to_sfu1",
			Help: "Latency from SFU 2 to SFU 1",
		},
		[]string{"latency_sfu2_to_sfu1"},
	),
	LatencySfu1ToClient1: prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webrtc_latency_sfu1_to_client1",
			Help: "Latency from SFU 1 to client 1",
		},
		[]string{"latency_sfu1_to_client1"},
	),
}

func init() {
	// we need to register the counter so prometheus can collect this metric
	log.Println("init() function called")
	prometheus.MustRegister(
		stats.FrameID,
		stats.MessageSentTimeClient2,
		stats.MessageSentTimeSfu2,
		stats.MessageSentTimeSfu1,
		stats.MessageSentTimeClient1,
		stats.LatencyClient2ToSfu2,
		stats.LatencySfu2ToSfu1,
		stats.LatencySfu1ToClient1,
		stats.LatencyEndToEnd,
	)
}

func main() {

	httpPromServer()
	// Assert that we have an audio or video file
	_, err := os.Stat(videoFileName)
	haveVideoFile := !os.IsNotExist(err)

	_, err = os.Stat(audioFileName)
	haveAudioFile := !os.IsNotExist(err)

	if !haveAudioFile && !haveVideoFile {
		panic("Could not find `" + audioFileName + "` or `" + videoFileName + "`")
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	dataChannel, err := peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		panic(err)
	}

	var NewHybridClock *fastclock.HybridClock
	_ = NewHybridClock
	// Register channel opening handling
	dataChannel.OnOpen(func() {
		NewHybridClock = fastclock.NewHybridClock()
		fmt.Printf(
			"Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n",
			dataChannel.Label(), dataChannel.ID(),
		)

	})
	logChan := make(chan DataChannelMessage, 100)
	go func() {
		file, err := os.Create("datachannel_messages.csv")
		if err != nil {
			panic(err)
		}
		defer file.Close()

		writer := csv.NewWriter(file)
		defer writer.Flush()

		// Write header once
		writer.Write([]string{"sep=,"}) // This line is to ensure Excel opens the file correctly
		writer.Write([]string{
			"FrameID", "MessageSentTimeClient2", "MessageSentTimeSfu2", "MessageSentTimeSfu1",
			"MessageSentTimeClient1", "MessageSendRate", "JitterSFU2", "JitterSFU1", "LatencyEndToEnd",
		})

		for msg := range logChan {
			// write to csv
			record := []string{
				strconv.FormatInt(msg.FrameID, 10),
				strconv.FormatInt(msg.MessageSentTimeClient2, 10),
				strconv.FormatInt(msg.MessageSentTimeSfu2, 10),
				strconv.FormatInt(msg.MessageSentTimeSfu1, 10),
				strconv.FormatInt(msg.MessageSentTimeClient1, 10),
				strconv.FormatInt(msg.MessageSendRate, 10),
				strconv.FormatInt(msg.JitterSFU2, 10),
				strconv.FormatInt(msg.JitterSFU1, 10),
				strconv.FormatInt(msg.LatencyEndToEnd, 10),
			}
			writer.Write(record)
			writer.Flush() // Optional: remove or batch flush for better performance
		}
	}()

	// Register text message handling
	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		var frameData DataChannelMessage
		err := json.Unmarshal(msg.Data, &frameData)
		if err != nil {
			fmt.Println("Error unmarshalling:", err)
			return
		}
		// frameData.MessageSentTimeClient1 = NewHybridClock.Now().UnixMilli()
		frameData.MessageSentTimeClient1 = time.Now().UnixMilli()
		stats.FrameID.WithLabelValues("FrameID").Set(float64(frameData.FrameID))
		stats.MessageSentTimeClient2.WithLabelValues("MessageSentTimeClient2").Set(float64(frameData.MessageSentTimeClient2))
		stats.MessageSentTimeSfu2.WithLabelValues("MessageSentTimeSfu2").Set(float64(frameData.MessageSentTimeSfu2))
		stats.MessageSentTimeSfu1.WithLabelValues("MessageSentTimeSfu1").Set(float64(frameData.MessageSentTimeSfu1))
		stats.MessageSentTimeClient1.WithLabelValues("MessageSentTimeClient1").Set(float64(frameData.MessageSentTimeClient1))
		stats.LatencyEndToEnd.WithLabelValues("LatencyEndToEnd").Set(
			float64(frameData.MessageSentTimeClient1 - frameData.MessageSentTimeClient2))
		stats.LatencyClient2ToSfu2.WithLabelValues("LatencyClient2ToSfu2").Set(
			float64(frameData.MessageSentTimeSfu2 - frameData.MessageSentTimeClient2),
		)
		stats.LatencySfu2ToSfu1.WithLabelValues("LatencySfu2ToSfu1").Set(
			float64(frameData.MessageSentTimeSfu1 - frameData.MessageSentTimeSfu2),
		)
		stats.LatencySfu1ToClient1.WithLabelValues("LatencySfu1ToClient1").Set(
			float64(frameData.MessageSentTimeClient1 - frameData.MessageSentTimeSfu1),
		)
		frameData.LatencyEndToEnd = frameData.MessageSentTimeClient1 - frameData.MessageSentTimeClient2
		// fmt.Printf("Message from DataChannel '%s': \n frameID: '%d', client2: '%d', sfu2: '%d', sfu1: '%d', client1: '%d'\n", dataChannel.Label(), frameData.FrameID, frameData.MessageSentTimeClient2, frameData.MessageSentTimeSfu2, frameData.MessageSentTimeSfu1, frameData.MessageSentTimeClient1)
		logChan <- frameData
	})

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())

	if haveVideoFile {
		file, openErr := os.Open(videoFileName)
		if openErr != nil {
			panic(openErr)
		}

		_, header, openErr := ivfreader.NewWith(file)
		if openErr != nil {
			panic(openErr)
		}

		// Determine video codec
		var trackCodec string
		switch header.FourCC {
		case "AV01":
			trackCodec = webrtc.MimeTypeAV1
		case "VP90":
			trackCodec = webrtc.MimeTypeVP9
		case "VP80":
			trackCodec = webrtc.MimeTypeVP8
		default:
			panic(fmt.Sprintf("Unable to handle FourCC %s", header.FourCC))
		}

		// Create a video track
		videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: trackCodec}, "video", "pion",
		)
		if videoTrackErr != nil {
			panic(videoTrackErr)
		}

		rtpSender, videoTrackErr := peerConnection.AddTrack(videoTrack)
		if videoTrackErr != nil {
			panic(videoTrackErr)
		}

		// Read incoming RTCP packets
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()

		go func() {
			// Open a IVF file and start reading using our IVFReader
			file, ivfErr := os.Open(videoFileName)
			if ivfErr != nil {
				panic(ivfErr)
			}

			for {

				if _, err := file.Seek(0, io.SeekStart); err != nil {
					panic(fmt.Errorf("failed to rewind file: %w", err))
				}

				ivf, header, ivfErr := ivfreader.NewWith(file)
				if ivfErr != nil {
					panic(ivfErr)
				}

				// Wait for connection established
				<-iceConnectedCtx.Done()
				// Send our video file frame at a time
				ticker := time.NewTicker(
					time.Millisecond * time.Duration((float32(header.TimebaseNumerator)/float32(header.TimebaseDenominator))*1000),
				)
				defer ticker.Stop()
				for ; true; <-ticker.C {
					frame, _, ivfErr := ivf.ParseNextFrame()
					if errors.Is(ivfErr, io.EOF) {
						fmt.Println("Reached end of video, restarting...")
						break
					}

					if ivfErr != nil {
						panic(ivfErr)
					}

					if ivfErr = videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); ivfErr != nil {
						panic(ivfErr)
					}
				}

			}
		}()
	}

	if haveAudioFile {
		// Create an audio track
		audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion",
		)
		if audioTrackErr != nil {
			panic(audioTrackErr)
		}

		rtpSender, audioTrackErr := peerConnection.AddTrack(audioTrack)
		if audioTrackErr != nil {
			panic(audioTrackErr)
		}

		// Read incoming RTCP packets
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()

		go func() {
			// Open an OGG file and start reading using our OGGReader
			file, oggErr := os.Open(audioFileName)
			if oggErr != nil {
				panic(oggErr)
			}

			ogg, _, oggErr := oggreader.NewWith(file)
			if oggErr != nil {
				panic(oggErr)
			}

			// Wait for connection established
			<-iceConnectedCtx.Done()

			// Keep track of last granule
			var lastGranule uint64

			// Send audio in a timely manner
			ticker := time.NewTicker(oggPageDuration)
			defer ticker.Stop()
			for ; true; <-ticker.C {
				pageData, pageHeader, oggErr := ogg.ParseNextPage()
				if errors.Is(oggErr, io.EOF) {
					fmt.Printf("All audio pages parsed and sent")
					os.Exit(0)
				}

				if oggErr != nil {
					panic(oggErr)
				}

				// The amount of samples is the difference between the last and current timestamp
				sampleCount := float64(pageHeader.GranulePosition - lastGranule)
				lastGranule = pageHeader.GranulePosition
				sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

				if oggErr = audioTrack.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); oggErr != nil {
					panic(oggErr)
				}
			}
		}()
	}

	// Set the handler for ICE connection state change
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			iceConnectedCtxCancel()
		}
	})

	// Set the handler for Peer connection state change
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", state.String())

		if state == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}

		if state == webrtc.PeerConnectionStateClosed {
			fmt.Println("Peer Connection has gone to closed exiting")
			os.Exit(0)
		}
	})

	// Create offer
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	if err := peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}

	// Send offer to HTTP endpoint
	offerBase64 := encode(&offer)
	resp, err := http.Post(httpEndpoint, "application/json", strings.NewReader(offerBase64))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	// Read the response containing the answer
	var answerBase64 string
	println("Waiting for answer...", resp.Status, resp.Body)
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	answerBase64 = strings.TrimSpace(string(respBody))
	// Decode the answer and set it as the remote description
	var answer webrtc.SessionDescription
	decode(answerBase64, &answer)

	fmt.Println("Answer received, setting remote description...", answerBase64, answer)
	if err := peerConnection.SetRemoteDescription(answer); err != nil {
		panic(err)
	}

	// Create answer
	// answer, err = peerConnection.CreateAnswer(nil)
	// if err != nil {
	// 	panic(err)
	// }

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	// if err := peerConnection.SetLocalDescription(answer); err != nil {
	// 	panic(err)
	// }

	// Block until ICE Gathering is complete, disabling trickle ICE
	<-gatherComplete

	// Output the answer in base64 so we can paste it in the browser
	fmt.Println(encode(peerConnection.LocalDescription()))

	// Block forever
	select {}
}

func httpPromServer() {
	mux_s1 := http.NewServeMux()
	mux_s1.Handle("/metrics", promhttp.Handler())

	go func() {
		// nolint: gosec
		panic(http.ListenAndServe(":"+strconv.Itoa(8080), mux_s1))
	}()
}

// Read from stdin until we get a newline.
func readUntilNewline() (in string) {
	var err error

	r := bufio.NewReader(os.Stdin)
	for {
		in, err = r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			panic(err)
		}

		if in = strings.TrimSpace(in); len(in) > 0 {
			break
		}
	}

	fmt.Println("")

	return
}

// JSON encode + base64 a SessionDescription.
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription.
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
