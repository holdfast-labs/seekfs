//go:build !uiautomation

package main

// uiauto_fixture_stub.go: production builds never read the UI-drive fixture
// environment or replace the volume/index seams.

func installUIAutoFixtureService() {}

func fixtureCapabilities(caps serviceCapabilities) serviceCapabilities { return caps }

func uiDriveFixtureActive() bool { return false }
