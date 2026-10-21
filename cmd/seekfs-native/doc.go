// Package main implements seekfs-native, a GPU-rendered native desktop UI
// for the seekfs resident file-search service, built with Gio (gioui.org).
//
// It talks to the service over its named pipe using the same JSON protocol
// as the other UIs (serviceRequest/serviceResponse with jsonResult rows).
// The service side is unchanged.
//
// Layout:
//   - protocol.go, client*.go: wire types + named-pipe client.
//   - query.go, snippet.go: query normalization, Contents-toggle rewrite,
//     snippet highlight segmentation.
//   - model.go: result rows, client-side sorting/grouping.
//   - settings.go: window/view persistence (%APPDATA%\seekfs\native.json).
//   - searcher.go, history.go: debounced async search, back/forward.
//   - gtheme.go: design system (schemes, Segoe UI, dp scaling).
//   - gmain.go, gstate.go, gactions.go: event loop, state, verbs.
//   - gtoolbar.go, gsidebar.go, gresults.go, gpreview.go, gstatus.go,
//     gwidgets.go: the interface.
//   - gshell*.go + Win32 files: shell integrations (context menu, icons,
//     drag-out, Recycle Bin, frame theming).
package main
