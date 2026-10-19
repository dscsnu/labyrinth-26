package qr

// The qr use case has no SQL: it holds no state, so there is no db struct and
// no db.go methods. Every token is derived on demand from the secret, the
// waypoint ID and the issue time (see read.go and write.go).
