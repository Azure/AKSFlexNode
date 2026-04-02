package aksmachine

// Redact removes sensitive fields from the action.
func (x *EnsureMachine) Redact() {
	// Redact the service principal client secret carried in the spec.
	if sp := x.GetSpec().GetAzureCredential().GetServicePrincipal(); sp != nil {
		sp.SetClientSecret("")
	}
}
