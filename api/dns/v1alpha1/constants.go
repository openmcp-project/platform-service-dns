package v1alpha1

import (
	openmcpconst "github.com/openmcp-project/openmcp-operator/api/constants"
)

const (
	OperationAnnotation = "dns." + openmcpconst.OperationAnnotation

	ExternalDNSFinalizerOnCluster = "platformservice." + openmcpconst.OpenMCPGroupName + "/dns"

	ReasonTargetClusterInteractionProblem = "TargetClusterInteractionProblem"
)
