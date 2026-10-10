package rules

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Short names for the generic decoder in rules_test.go.
type (
	appsDeployment  = appsv1.Deployment
	appsStatefulSet = appsv1.StatefulSet
	appsDaemonSet   = appsv1.DaemonSet
	batchJob        = batchv1.Job
	gwGateway       = gatewayv1.Gateway
	gwHTTPRoute     = gatewayv1.HTTPRoute
)
