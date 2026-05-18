package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/redpanda-data/benthos/v4/public/bloblang"
	_ "github.com/redpanda-data/connect/public/bundle/free/v4"
	"github.com/tliron/commonlog"
	_ "github.com/tliron/commonlog/simple"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
	"github.com/tliron/glsp/server"
)

const lsName = "bloblang"

var version = "0.0.1"

func main() {
	log.Printf("[main] Starting LSP server...")

	commonlog.Configure(1, nil)
	handler := protocol.Handler{
		Initialize:            initialize,
		Initialized:           initialized,
		Shutdown:              shutdown,
		SetTrace:              setTrace,
		TextDocumentDidOpen:   didOpen,
		TextDocumentDidChange: didChange,
		TextDocumentDidClose:  didClose,
	}

	s := server.NewServer(&handler, lsName, false)
	s.RunStdio()
}

func initialize(context *glsp.Context, params *protocol.InitializeParams) (any, error) {
	log.Printf("[initialize] received event")
	capabilities := handlerCapabilities()

	return protocol.InitializeResult{
		Capabilities: capabilities,
		ServerInfo: &protocol.InitializeResultServerInfo{
			Name:    lsName,
			Version: &version,
		},
	}, nil
}

func initialized(context *glsp.Context, params *protocol.InitializedParams) error {
	log.Printf("[initialized] received event")
	return nil
}

func shutdown(context *glsp.Context) error {
	log.Printf("[shutdown] received event")
	return nil
}

func setTrace(context *glsp.Context, params *protocol.SetTraceParams) error {
	log.Printf("[setTrace] received event")
	protocol.SetTraceValue(params.Value)
	return nil
}

func handlerCapabilities() protocol.ServerCapabilities {
	openClose := true
	syncKind := protocol.TextDocumentSyncKindFull

	return protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: &openClose,
			Change:    &syncKind,
		},
	}
}

func didOpen(ctx *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	uri := params.TextDocument.URI
	text := params.TextDocument.Text

	log.Printf("[didOpen] received event: %s", uri)
	log.Printf("[didOpen] extracted content: %d bytes", len(text))

	validateMapping(ctx, uri, text)

	return nil
}

func didChange(ctx *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	uri := params.TextDocument.URI

	log.Printf("[didChange] received event: %s", uri)

	if len(params.ContentChanges) == 0 {
		log.Printf("[didChange] no changes made")
		return nil
	}

	var text string
	switch change := params.ContentChanges[0].(type) {
	case protocol.TextDocumentContentChangeEvent:
		text = change.Text
	case protocol.TextDocumentContentChangeEventWhole:
		text = change.Text
	}

	log.Printf("[didChange] extracted content: %d bytes", len(text))

	validateMapping(ctx, uri, text)

	return nil
}

func didClose(ctx *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	log.Printf("[didClose] received event: %s", params.TextDocument.URI)
	return nil
}

func uriToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}

	// For file:// URIs
	if u.Scheme != "file" {
		return "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}

	path := u.Path

	// Windows fix: strip leading slash like /C:/...
	if os.PathSeparator == '\\' && len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	return filepath.FromSlash(path), nil
}

func customImporter(uri string, name string) ([]byte, error) {
	basePath, err := uriToPath(uri)

	if err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}

	return os.ReadFile(filepath.Join(path.Dir(basePath), name))
}

func validateMapping(ctx *glsp.Context, uri string, text string) {
	log.Printf("[validateMapping] starting validation for %s", uri)

	if text == "" {
		log.Printf("[validateMapping] empty content, clearing diagnostics")
		publishDiagnostics(ctx, uri, nil)
		return
	}

	log.Printf("[validateMapping] initializing environment")
	env := bloblang.GlobalEnvironment().WithCustomImporter(func(name string) ([]byte, error) {
		return customImporter(uri, name)
	})

	log.Printf("[validateMapping] parsing mapping")
	_, err := env.Parse(text)

	if err == nil {
		log.Printf("[validateMapping] no errors found")
		publishDiagnostics(ctx, uri, nil)
		return
	}

	log.Printf("[validateMapping] error found: %v", err)
	log.Printf("[validateMapping] converting error to diagnostics")
	diagnostics := convertErrorToDiagnostics(text, err)

	publishDiagnostics(ctx, uri, diagnostics)
}

func convertErrorToDiagnostics(text string, err error) []protocol.Diagnostic {
	severity := protocol.DiagnosticSeverityError
	source := "bloblang"

	rawErr := ""
	prettyErr, ok := err.(*bloblang.ParseError)

	if ok {
		rawErr = prettyErr.ErrorMultiline()
	} else {
		rawErr = err.Error()
	}

	log.Printf("[convertError] raw error: %s", rawErr)

	var line, char int
	// Bloblang errors typically look like "failed to parse mapping: line 6 char 26: ..."
	// We search for the "line X char Y" pattern in the error string.
	idx := strings.Index(rawErr, "line ")
	if idx == -1 {
		log.Printf("[convertError] could not find line/char pattern in error, using fallback")
		return []protocol.Diagnostic{
			{
				Range:    protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 0}},
				Severity: &severity,
				Message:  rawErr,
				Source:   &source,
			},
		}
	}

	_, err = fmt.Sscanf(rawErr[idx:], "line %d char %d", &line, &char)
	if err != nil {
		log.Printf("[convertError] failed to parse line/char from error: %v", err)
		return []protocol.Diagnostic{
			{
				Range:    protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 0}},
				Severity: &severity,
				Message:  err.Error(),
				Source:   &source,
			},
		}
	}

	cleanMessage := rawErr
	if parts := strings.SplitN(rawErr, ": ", 2); len(parts) == 2 {
		cleanMessage = parts[1]
	}
	cleanMessage = strings.Split(cleanMessage, "\n")[0]

	if line > 0 {
		line--
	}
	if char > 0 {
		char--
	}

	return []protocol.Diagnostic{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: protocol.UInteger(line), Character: protocol.UInteger(char)},
				End:   protocol.Position{Line: protocol.UInteger(line), Character: protocol.UInteger(char)},
			},
			Severity: &severity,
			Message:  cleanMessage,
			Source:   &source,
		},
	}
}

func publishDiagnostics(ctx *glsp.Context, uri string, diagnostics []protocol.Diagnostic) {
	if diagnostics == nil {
		diagnostics = []protocol.Diagnostic{}
	}

	log.Printf("[publishDiagnostics] publishing %d diagnostics for %s", len(diagnostics), uri)
	ctx.Notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: diagnostics,
	})
}
