/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

// This file contains a collection of methods that can be used from go-restful to
// generate Swagger API documentation for its models. Please read this PR for more
// information on the implementation: https://github.com/emicklei/go-restful/pull/215
//
// TODOs are ignored from the parser (e.g. TODO(andronat):... || TODO:...) if and only if
// they are on one line! For multiple line or blocks that you want to ignore use ---.
// Any context after a --- is ignored.
//
// Those methods can be generated by using hack/update-codegen.sh

// AUTO-GENERATED FUNCTIONS START HERE. DO NOT EDIT.
var map_FieldSelectorAttributes = map[string]string{
	"":             "FieldSelectorAttributes indicates a field limited access. Webhook authors are encouraged to * ensure rawSelector and requirements are not both set * consider the requirements field if set * not try to parse or consider the rawSelector field if set. This is to avoid another CVE-2022-2880 (i.e. getting different systems to agree on how exactly to parse a query is not something we want), see https://www.oxeye.io/resources/golang-parameter-smuggling-attack for more details. For the *SubjectAccessReview endpoints of the kube-apiserver: * If rawSelector is empty and requirements are empty, the request is not limited. * If rawSelector is present and requirements are empty, the rawSelector will be parsed and limited if the parsing succeeds. * If rawSelector is empty and requirements are present, the requirements should be honored * If rawSelector is present and requirements are present, the request is invalid.",
	"rawSelector":  "rawSelector is the serialization of a field selector that would be included in a query parameter. Webhook implementations are encouraged to ignore rawSelector. The kube-apiserver's *SubjectAccessReview will parse the rawSelector as long as the requirements are not present.",
	"requirements": "requirements is the parsed interpretation of a field selector. All requirements must be met for a resource instance to match the selector. Webhook implementations should handle requirements, but how to handle them is up to the webhook. Since requirements can only limit the request, it is safe to authorize as unlimited request if the requirements are not understood.",
}

func (FieldSelectorAttributes) SwaggerDoc() map[string]string {
	return map_FieldSelectorAttributes
}

var map_LabelSelectorAttributes = map[string]string{
	"":             "LabelSelectorAttributes indicates a label limited access. Webhook authors are encouraged to * ensure rawSelector and requirements are not both set * consider the requirements field if set * not try to parse or consider the rawSelector field if set. This is to avoid another CVE-2022-2880 (i.e. getting different systems to agree on how exactly to parse a query is not something we want), see https://www.oxeye.io/resources/golang-parameter-smuggling-attack for more details. For the *SubjectAccessReview endpoints of the kube-apiserver: * If rawSelector is empty and requirements are empty, the request is not limited. * If rawSelector is present and requirements are empty, the rawSelector will be parsed and limited if the parsing succeeds. * If rawSelector is empty and requirements are present, the requirements should be honored * If rawSelector is present and requirements are present, the request is invalid.",
	"rawSelector":  "rawSelector is the serialization of a field selector that would be included in a query parameter. Webhook implementations are encouraged to ignore rawSelector. The kube-apiserver's *SubjectAccessReview will parse the rawSelector as long as the requirements are not present.",
	"requirements": "requirements is the parsed interpretation of a label selector. All requirements must be met for a resource instance to match the selector. Webhook implementations should handle requirements, but how to handle them is up to the webhook. Since requirements can only limit the request, it is safe to authorize as unlimited request if the requirements are not understood.",
}

func (LabelSelectorAttributes) SwaggerDoc() map[string]string {
	return map_LabelSelectorAttributes
}

var map_LocalSubjectAccessReview = map[string]string{
	"":         "LocalSubjectAccessReview checks whether or not a user or group can perform an action in a given namespace. Having a namespace scoped resource makes it much easier to grant namespace scoped policy that includes permissions checking.",
	"metadata": "Standard list metadata. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata",
	"spec":     "Spec holds information about the request being evaluated.  spec.namespace must be equal to the namespace you made the request against.  If empty, it is defaulted.",
	"status":   "Status is filled in by the server and indicates whether the request is allowed or not",
}

func (LocalSubjectAccessReview) SwaggerDoc() map[string]string {
	return map_LocalSubjectAccessReview
}

var map_NonResourceAttributes = map[string]string{
	"":     "NonResourceAttributes includes the authorization attributes available for non-resource requests to the Authorizer interface",
	"path": "Path is the URL path of the request",
	"verb": "Verb is the standard HTTP verb",
}

func (NonResourceAttributes) SwaggerDoc() map[string]string {
	return map_NonResourceAttributes
}

var map_NonResourceRule = map[string]string{
	"":                "NonResourceRule holds information that describes a rule for the non-resource",
	"verbs":           "Verb is a list of kubernetes non-resource API verbs, like: get, post, put, delete, patch, head, options.  \"*\" means all.",
	"nonResourceURLs": "NonResourceURLs is a set of partial urls that a user should have access to.  *s are allowed, but only as the full, final step in the path.  \"*\" means all.",
}

func (NonResourceRule) SwaggerDoc() map[string]string {
	return map_NonResourceRule
}

var map_ResourceAttributes = map[string]string{
	"":              "ResourceAttributes includes the authorization attributes available for resource requests to the Authorizer interface",
	"namespace":     "Namespace is the namespace of the action being requested.  Currently, there is no distinction between no namespace and all namespaces \"\" (empty) is defaulted for LocalSubjectAccessReviews \"\" (empty) is empty for cluster-scoped resources \"\" (empty) means \"all\" for namespace scoped resources from a SubjectAccessReview or SelfSubjectAccessReview",
	"verb":          "Verb is a kubernetes resource API verb, like: get, list, watch, create, update, delete, proxy.  \"*\" means all.",
	"group":         "Group is the API Group of the Resource.  \"*\" means all.",
	"version":       "Version is the API Version of the Resource.  \"*\" means all.",
	"resource":      "Resource is one of the existing resource types.  \"*\" means all.",
	"subresource":   "Subresource is one of the existing resource types.  \"\" means none.",
	"name":          "Name is the name of the resource being requested for a \"get\" or deleted for a \"delete\". \"\" (empty) means all.",
	"fieldSelector": "fieldSelector describes the limitation on access based on field.  It can only limit access, not broaden it.",
	"labelSelector": "labelSelector describes the limitation on access based on labels.  It can only limit access, not broaden it.",
}

func (ResourceAttributes) SwaggerDoc() map[string]string {
	return map_ResourceAttributes
}

var map_ResourceRule = map[string]string{
	"":              "ResourceRule is the list of actions the subject is allowed to perform on resources. The list ordering isn't significant, may contain duplicates, and possibly be incomplete.",
	"verbs":         "Verb is a list of kubernetes resource API verbs, like: get, list, watch, create, update, delete, proxy.  \"*\" means all.",
	"apiGroups":     "APIGroups is the name of the APIGroup that contains the resources.  If multiple API groups are specified, any action requested against one of the enumerated resources in any API group will be allowed.  \"*\" means all.",
	"resources":     "Resources is a list of resources this rule applies to.  \"*\" means all in the specified apiGroups.\n \"*/foo\" represents the subresource 'foo' for all resources in the specified apiGroups.",
	"resourceNames": "ResourceNames is an optional white list of names that the rule applies to.  An empty set means that everything is allowed.  \"*\" means all.",
}

func (ResourceRule) SwaggerDoc() map[string]string {
	return map_ResourceRule
}

var map_SelfSubjectAccessReview = map[string]string{
	"":         "SelfSubjectAccessReview checks whether or the current user can perform an action.  Not filling in a spec.namespace means \"in all namespaces\".  Self is a special case, because users should always be able to check whether they can perform an action",
	"metadata": "Standard list metadata. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata",
	"spec":     "Spec holds information about the request being evaluated.  user and groups must be empty",
	"status":   "Status is filled in by the server and indicates whether the request is allowed or not",
}

func (SelfSubjectAccessReview) SwaggerDoc() map[string]string {
	return map_SelfSubjectAccessReview
}

var map_SelfSubjectAccessReviewSpec = map[string]string{
	"":                      "SelfSubjectAccessReviewSpec is a description of the access request.  Exactly one of ResourceAuthorizationAttributes and NonResourceAuthorizationAttributes must be set",
	"resourceAttributes":    "ResourceAuthorizationAttributes describes information for a resource access request",
	"nonResourceAttributes": "NonResourceAttributes describes information for a non-resource access request",
}

func (SelfSubjectAccessReviewSpec) SwaggerDoc() map[string]string {
	return map_SelfSubjectAccessReviewSpec
}

var map_SelfSubjectRulesReview = map[string]string{
	"":         "SelfSubjectRulesReview enumerates the set of actions the current user can perform within a namespace. The returned list of actions may be incomplete depending on the server's authorization mode, and any errors experienced during the evaluation. SelfSubjectRulesReview should be used by UIs to show/hide actions, or to quickly let an end user reason about their permissions. It should NOT Be used by external systems to drive authorization decisions as this raises confused deputy, cache lifetime/revocation, and correctness concerns. SubjectAccessReview, and LocalAccessReview are the correct way to defer authorization decisions to the API server.",
	"metadata": "Standard list metadata. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata",
	"spec":     "Spec holds information about the request being evaluated.",
	"status":   "Status is filled in by the server and indicates the set of actions a user can perform.",
}

func (SelfSubjectRulesReview) SwaggerDoc() map[string]string {
	return map_SelfSubjectRulesReview
}

var map_SelfSubjectRulesReviewSpec = map[string]string{
	"":          "SelfSubjectRulesReviewSpec defines the specification for SelfSubjectRulesReview.",
	"namespace": "Namespace to evaluate rules for. Required.",
}

func (SelfSubjectRulesReviewSpec) SwaggerDoc() map[string]string {
	return map_SelfSubjectRulesReviewSpec
}

var map_SubjectAccessReview = map[string]string{
	"":         "SubjectAccessReview checks whether or not a user or group can perform an action.",
	"metadata": "Standard list metadata. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata",
	"spec":     "Spec holds information about the request being evaluated",
	"status":   "Status is filled in by the server and indicates whether the request is allowed or not",
}

func (SubjectAccessReview) SwaggerDoc() map[string]string {
	return map_SubjectAccessReview
}

var map_SubjectAccessReviewSpec = map[string]string{
	"":                      "SubjectAccessReviewSpec is a description of the access request.  Exactly one of ResourceAuthorizationAttributes and NonResourceAuthorizationAttributes must be set",
	"resourceAttributes":    "ResourceAuthorizationAttributes describes information for a resource access request",
	"nonResourceAttributes": "NonResourceAttributes describes information for a non-resource access request",
	"user":                  "User is the user you're testing for. If you specify \"User\" but not \"Groups\", then is it interpreted as \"What if User were not a member of any groups",
	"groups":                "Groups is the groups you're testing for.",
	"extra":                 "Extra corresponds to the user.Info.GetExtra() method from the authenticator.  Since that is input to the authorizer it needs a reflection here.",
	"uid":                   "UID information about the requesting user.",
}

func (SubjectAccessReviewSpec) SwaggerDoc() map[string]string {
	return map_SubjectAccessReviewSpec
}

var map_SubjectAccessReviewStatus = map[string]string{
	"":                "SubjectAccessReviewStatus",
	"allowed":         "Allowed is required. True if the action would be allowed, false otherwise.",
	"denied":          "Denied is optional. True if the action would be denied, otherwise false. If both allowed is false and denied is false, then the authorizer has no opinion on whether to authorize the action. Denied may not be true if Allowed is true.",
	"reason":          "Reason is optional.  It indicates why a request was allowed or denied.",
	"evaluationError": "EvaluationError is an indication that some error occurred during the authorization check. It is entirely possible to get an error and be able to continue determine authorization status in spite of it. For instance, RBAC can be missing a role, but enough roles are still present and bound to reason about the request.",
}

func (SubjectAccessReviewStatus) SwaggerDoc() map[string]string {
	return map_SubjectAccessReviewStatus
}

var map_SubjectRulesReviewStatus = map[string]string{
	"":                 "SubjectRulesReviewStatus contains the result of a rules check. This check can be incomplete depending on the set of authorizers the server is configured with and any errors experienced during evaluation. Because authorization rules are additive, if a rule appears in a list it's safe to assume the subject has that permission, even if that list is incomplete.",
	"resourceRules":    "ResourceRules is the list of actions the subject is allowed to perform on resources. The list ordering isn't significant, may contain duplicates, and possibly be incomplete.",
	"nonResourceRules": "NonResourceRules is the list of actions the subject is allowed to perform on non-resources. The list ordering isn't significant, may contain duplicates, and possibly be incomplete.",
	"incomplete":       "Incomplete is true when the rules returned by this call are incomplete. This is most commonly encountered when an authorizer, such as an external authorizer, doesn't support rules evaluation.",
	"evaluationError":  "EvaluationError can appear in combination with Rules. It indicates an error occurred during rule evaluation, such as an authorizer that doesn't support rule evaluation, and that ResourceRules and/or NonResourceRules may be incomplete.",
}

func (SubjectRulesReviewStatus) SwaggerDoc() map[string]string {
	return map_SubjectRulesReviewStatus
}

// AUTO-GENERATED FUNCTIONS END HERE
