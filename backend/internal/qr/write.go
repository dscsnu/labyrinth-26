package qr

import (
	"encoding/hex"
	"errors"
)

// ErrUnknownWaypoint means no waypoint has that ID, so a display can't be
// issued for it.
var ErrUnknownWaypoint = errors.New("unknown waypoint")

// SigningKey is what a /qr display fetches once: the waypoint-scoped HMAC key
// and the timing rules it needs to sign its own rotating tokens, so the
// display makes one request and then signs locally, no polling.
type SigningKey struct {
	// WaypointID is the 4-digit waypoint the display will sign for.
	WaypointID string `json:"waypoint_id"`
	// Key is the per-waypoint HMAC key, hex-encoded. The display signs
	// "<waypoint>:<unix seconds>" with it; the signature is base64url.
	Key string `json:"key"`
	// RotateMs is how often the display should sign a fresh token
	// (qr.rotate), in milliseconds.
	RotateMs int64 `json:"rotate_ms"`
	// WindowMs is how long each token is valid: qr.rotate + qr.tolerance,
	// in milliseconds.
	WindowMs int64 `json:"window_ms"`
	// ServerTime is the backend's clock in whole seconds when this was
	// issued, so the display can correct its own clock before signing.
	ServerTime int64 `json:"server_time"`
}

// SigningKey returns the waypoint-scoped key and rules for waypointID.
// ErrUnknownWaypoint if no waypoint has that ID.
func (v *Verifier) SigningKey(waypointID string) (SigningKey, error) {
	if _, ok := v.atlas.WaypointIndex(waypointID); !ok {
		return SigningKey{}, ErrUnknownWaypoint
	}
	return SigningKey{
		WaypointID: waypointID,
		Key:        hex.EncodeToString(v.keyFor(waypointID)),
		RotateMs:   v.rules.Rotate.Milliseconds(),
		WindowMs:   v.window().Milliseconds(),
		ServerTime: v.now().Unix(),
	}, nil
}
