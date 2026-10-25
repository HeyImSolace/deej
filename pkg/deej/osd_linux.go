package deej

// showVolumeOSD is a no-op on Linux; desktop environments usually show their own volume OSD
func showVolumeOSD(label string, value float32, position string, mic bool, sessions []Session) {}
