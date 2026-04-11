package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AIInsightSpec defines a request for AI-generated operational insight about a K8s resource
type AIInsightSpec struct {
	// Target resource to analyze
	Target AIInsightTarget `json:"target"`

	// Type of analysis requested
	// +kubebuilder:validation:Enum=Diagnose;Audit;Recommend;Explain
	AnalysisType string `json:"analysisType"`

	// TTLSeconds — how long to keep the result before the CR is auto-deleted
	// +optional
	// +kubebuilder:default=3600
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`

	// Context hints to inject into the AI prompt (e.g. recent deploy SHA, incident ID)
	// +optional
	ContextHints map[string]string `json:"contextHints,omitempty"`
}

type AIInsightTarget struct {
	// Kind: Pod, Deployment, Node, or Event
	// +kubebuilder:validation:Enum=Pod;Deployment;Node;Event
	Kind string `json:"kind"`

	// Name of the target resource
	Name string `json:"name"`

	// Namespace of the target resource
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// AIInsightStatus holds the AI-generated analysis result
type AIInsightStatus struct {
	// Phase: Pending | Running | Completed | Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Summary is the AI-generated analysis (markdown)
	// +optional
	Summary string `json:"summary,omitempty"`

	// Model used for the analysis
	// +optional
	Model string `json:"model,omitempty"`

	// Tokens consumed by this analysis
	// +optional
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`

	// CompletedAt is when the analysis finished
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// FailureReason explains why the analysis failed
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// Conditions represents the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.analysisType`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AIInsight is the Schema for the aiinsights API
type AIInsight struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AIInsightSpec   `json:"spec,omitempty"`
	Status AIInsightStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AIInsightList contains a list of AIInsight
type AIInsightList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AIInsight `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AIInsight{}, &AIInsightList{})
}
