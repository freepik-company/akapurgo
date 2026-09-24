package v1alpha1

import "go.uber.org/zap"

// Context TODO
type Context struct {
	Config *ConfigSpec
	Logger *zap.SugaredLogger
}

type PurgeRequest struct {
	PurgeType          string `json:"purgeType"`   // "urls" or "cache-tags"
	ActionType         string `json:"actionType"`  // "invalidate" or "delete"
	Environment        string `json:"environment"` // "production" or "staging"
	OriginPurgeRequest bool   `json:"originPurgeRequest,omitempty"`
	// PostPurgeRequest is kept for API compatibility. It now triggers the same
	// pre-Akamai origin purge as OriginPurgeRequest.
	PostPurgeRequest bool     `json:"postPurgeRequest,omitempty"`
	ImBypass         bool     `json:"imBypass,omitempty"` // true or false
	Paths            []string `json:"paths"`
}

type AkamaiResponse struct {
	HTTPStatus int    `json:"httpStatus"`
	Detail     string `json:"detail"`
}
