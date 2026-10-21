// Package tests holds the black-box tests of internal/admin: the parts admin
// adds itself (the teams overview, the own-role rule, the search limit),
// against fakes of the use cases it calls. The routes end to end, against
// the real use cases, are tested in internal/server/tests (admin_test.go).
//
// Run with: go test ./internal/admin/...
package tests
