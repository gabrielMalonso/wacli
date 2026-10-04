package store

// ValidateOutboundObservation validates an IPC assertion's public scope. It
// does not establish that the observation occurred or was retained remotely.
func ValidateOutboundObservation(o OutboundOperation, f OutboundObservation) error {
	return validateOutboundObservation(o, f)
}
