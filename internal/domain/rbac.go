package domain

// Role represents a principal's access role.
type Role string

const (
	RoleOperator    Role = "operator"
	RoleGroupAdmin  Role = "group_admin"
	RoleSchoolAdmin Role = "school_admin"
	RoleTeacher     Role = "teacher"
	RoleReviewer    Role = "reviewer"
	RoleScanner     Role = "scanner"
)

// Action represents an operation that may be performed.
type Action string

const (
	ActionManageTenants    Action = "manage_tenants"
	ActionManageUsers      Action = "manage_users"
	ActionEditTunables     Action = "edit_tunables"
	ActionSubmitExam       Action = "submit_exam"
	ActionViewResults      Action = "view_results"
	ActionReviewFixApprove Action = "review_fix_approve"
)

// rolePermissions maps each role to the set of actions it may perform.
var rolePermissions = map[Role]map[Action]bool{
	RoleOperator: {
		ActionManageTenants:    true,
		ActionManageUsers:      true,
		ActionEditTunables:     true,
		ActionSubmitExam:       true,
		ActionViewResults:      true,
		ActionReviewFixApprove: true,
	},
	RoleGroupAdmin: {
		ActionManageUsers:      true,
		ActionEditTunables:     true,
		ActionSubmitExam:       true,
		ActionViewResults:      true,
		ActionReviewFixApprove: true,
	},
	RoleSchoolAdmin: {
		ActionManageUsers:      true,
		ActionEditTunables:     true,
		ActionSubmitExam:       true,
		ActionViewResults:      true,
		ActionReviewFixApprove: true,
	},
	RoleTeacher: {
		ActionSubmitExam:       true,
		ActionViewResults:      true,
		ActionReviewFixApprove: true,
	},
	RoleReviewer: {
		ActionViewResults:      true,
		ActionReviewFixApprove: true,
	},
	RoleScanner: {
		ActionSubmitExam: true,
	},
}

// ResourceCtx holds tenant context for authorization decisions.
type ResourceCtx struct {
	PrincipalTenants []string // tenants this principal belongs to
	ResourceTenantID string   // tenant of the target resource
	DeployMode       string   // "saas" or "onprem"
}

// Can reports whether any of the given roles may perform action on a resource.
// Tenant isolation: the principal must own the resource's tenant unless the
// principal holds the operator role AND DeployMode == "saas".
func Can(roles []Role, action Action, rctx ResourceCtx) bool {
	hasAction := false
	isOperator := false
	for _, r := range roles {
		if perms, ok := rolePermissions[r]; ok && perms[action] {
			hasAction = true
		}
		if r == RoleOperator {
			isOperator = true
		}
	}
	if !hasAction {
		return false
	}
	// Operator in saas mode can cross tenant boundaries.
	if isOperator && rctx.DeployMode == "saas" {
		return true
	}
	// Tenant isolation: principal must be in the resource's tenant.
	for _, t := range rctx.PrincipalTenants {
		if t == rctx.ResourceTenantID {
			return true
		}
	}
	return false
}
