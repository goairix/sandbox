package fuseprotocol

const (
	// MounterReadinessPort is the resident read-only HTTP TCP port on the Pod
	// network. It grants no management capability and carries no credentials.
	MounterReadinessPort = 9808
	// MounterReadinessPath accepts only a GET boolean readiness observation.
	// Positive readiness is not execution, flush or ownership-release proof.
	MounterReadinessPath = "/readyz"
)
