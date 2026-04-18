//go:build !unix

package ui

import "errors"

// Speaker view is unix-only because the transport uses unix-domain
// sockets and uid-scoped paths. The stubs below preserve the symbol
// shape so pager.go compiles on other platforms; every entry point
// refuses to start.

type stateMsg struct {
	Type    string
	Index   int
	Total   int
	Started int64
}

type advanceMsg struct {
	Type string
	Dir  string
}

type speakerServer struct{}

func startSpeakerServer(string) (*speakerServer, error) {
	return nil, errors.New("speaker view is unix-only")
}

func (*speakerServer) Broadcast(stateMsg)          {}
func (*speakerServer) Advances() <-chan advanceMsg { ch := make(chan advanceMsg); close(ch); return ch }
func (*speakerServer) Close() error                { return nil }

type speakerClient struct{}

func dialSpeaker(string) (*speakerClient, error) {
	return nil, errors.New("speaker view is unix-only")
}

func (*speakerClient) SendAdvance(string) error { return nil }
func (*speakerClient) States() <-chan stateMsg  { ch := make(chan stateMsg); close(ch); return ch }
func (*speakerClient) Close() error             { return nil }

func speakerSocketPath(string) (string, error) {
	return "", errors.New("speaker view is unix-only")
}
