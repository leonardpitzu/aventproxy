package utils

const (
	DirectionRecvonly = "recvonly"
	DirectionSendonly = "sendonly"
	DirectionSendRecv = "sendrecv"
)

const (
	KindVideo = "video"
	KindAudio = "audio"
)

// Media is one track the bridge asks a peer connection for.
type Media struct {
	Kind      string
	Direction string
}
