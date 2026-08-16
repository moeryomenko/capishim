//go:build tools

// Package tools pins build-time dependencies in go.mod. The modules listed
// here are imported for their side effect on the module graph; none of them
// are linked into the capishim binaries.
package tools

import (
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/utils/ptr"
	_ "sigs.k8s.io/yaml"
)
