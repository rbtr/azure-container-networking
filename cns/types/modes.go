package types

// ChannelMode CNS channel modes.
type ChannelMode string

const (
	Direct         ChannelMode = "Direct"
	Managed        ChannelMode = "Managed"
	CRD            ChannelMode = "CRD"
	MultiTenantCRD ChannelMode = "MultiTenantCRD"
)
