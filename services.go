package main

// startExtras starts the background helpers once the app is up.
func (m *Manager) startExtras() {
	m.startLAN()
	go m.watchLoop()
	go m.smoothLoop()
}
