package webrtc

import (
	"encoding/json"

	"github.com/pion/webrtc/v4"
)

// UnmarshalICEServers reads the ICE server list Tuya returns, whose "urls"
// field is either a single string or an array of them.
func UnmarshalICEServers(b []byte) ([]webrtc.ICEServer, error) {
	type iceServer struct {
		URLs       any    `json:"urls"`
		Username   string `json:"username,omitempty"`
		Credential string `json:"credential,omitempty"`
	}

	var src []iceServer
	if err := json.Unmarshal(b, &src); err != nil {
		return nil, err
	}

	dst := make([]webrtc.ICEServer, 0, len(src))
	for _, s := range src {
		srv := webrtc.ICEServer{Username: s.Username, Credential: s.Credential}
		switch v := s.URLs.(type) {
		case []string:
			srv.URLs = v
		case string:
			srv.URLs = []string{v}
		}
		dst = append(dst, srv)
	}

	return dst, nil
}
