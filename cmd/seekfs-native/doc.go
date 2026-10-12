// Package main implements seekfs-native, a fully native Win32 desktop UI
// for the seekfs resident file-search service.
//
// It talks to the service over its named pipe using the same JSON protocol
// as the Wails UI (cmd/seekfs/ui.go): serviceRequest/serviceResponse with
// jsonResult rows. The service side is unchanged.
//
// Layout:
//   - protocol.go, client_windows.go: wire types + named-pipe client.
//   - query.go, snippet.go: query normalization, Contents-toggle rewrite,
//     snippet highlight segmentation (ported from cmd/seekfs/ui.go and
//     cmd/seekfs/ui_frontend/query.js).
//   - model.go: result rows, client-side sorting/grouping.
//   - settings.go: window/view persistence (%APPDATA%\seekfs\native.json).
//   - searcher.go: debounced, sequence-numbered async search.
//   - *_windows.go: the Win32 UI (toolbar, sidebar, virtual ListView,
//     preview pane, shell integration).
package main
