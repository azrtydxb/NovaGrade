package api_test

import (
	"testing"

	"github.com/azrtydxb/novagrade/internal/domain"
)

func TestRBACMatrix(t *testing.T) {
	tenantA := "tenant-a"
	tenantB := "tenant-b"

	tests := []struct {
		name       string
		roles      []domain.Role
		action     domain.Action
		rctx       domain.ResourceCtx
		wantAllow  bool
	}{
		// Operator can do anything across tenants in saas mode
		{
			name:   "operator/saas/manage_tenants",
			roles:  []domain.Role{domain.RoleOperator},
			action: domain.ActionManageTenants,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantB,
				DeployMode:       "saas",
			},
			wantAllow: true,
		},
		{
			name:   "operator/onprem/cross_tenant_denied",
			roles:  []domain.Role{domain.RoleOperator},
			action: domain.ActionManageTenants,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantB,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		// Scanner can only submit_exam
		{
			name:   "scanner/submit_exam/same_tenant",
			roles:  []domain.Role{domain.RoleScanner},
			action: domain.ActionSubmitExam,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: true,
		},
		{
			name:   "scanner/view_results/denied",
			roles:  []domain.Role{domain.RoleScanner},
			action: domain.ActionViewResults,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		{
			name:   "scanner/manage_tenants/denied",
			roles:  []domain.Role{domain.RoleScanner},
			action: domain.ActionManageTenants,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		// Reviewer can view_results and review_fix_approve but not submit_exam
		{
			name:   "reviewer/view_results/allowed",
			roles:  []domain.Role{domain.RoleReviewer},
			action: domain.ActionViewResults,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: true,
		},
		{
			name:   "reviewer/submit_exam/denied",
			roles:  []domain.Role{domain.RoleReviewer},
			action: domain.ActionSubmitExam,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		// Teacher can submit, view, review
		{
			name:   "teacher/submit_exam",
			roles:  []domain.Role{domain.RoleTeacher},
			action: domain.ActionSubmitExam,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: true,
		},
		{
			name:   "teacher/manage_users/denied",
			roles:  []domain.Role{domain.RoleTeacher},
			action: domain.ActionManageUsers,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		// Group admin
		{
			name:   "group_admin/manage_users",
			roles:  []domain.Role{domain.RoleGroupAdmin},
			action: domain.ActionManageUsers,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: true,
		},
		{
			name:   "group_admin/manage_tenants/denied",
			roles:  []domain.Role{domain.RoleGroupAdmin},
			action: domain.ActionManageTenants,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		// School admin
		{
			name:   "school_admin/edit_tunables",
			roles:  []domain.Role{domain.RoleSchoolAdmin},
			action: domain.ActionEditTunables,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: true,
		},
		// Cross-tenant isolation (non-operator)
		{
			name:   "teacher/cross_tenant/denied",
			roles:  []domain.Role{domain.RoleTeacher},
			action: domain.ActionViewResults,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantB,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		// No roles
		{
			name:   "no_roles/denied",
			roles:  []domain.Role{},
			action: domain.ActionViewResults,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: false,
		},
		// Operator in onprem same tenant still allowed (has action + in tenant)
		{
			name:   "operator/onprem/same_tenant/allowed",
			roles:  []domain.Role{domain.RoleOperator},
			action: domain.ActionSubmitExam,
			rctx: domain.ResourceCtx{
				PrincipalTenants: []string{tenantA},
				ResourceTenantID: tenantA,
				DeployMode:       "onprem",
			},
			wantAllow: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.Can(tc.roles, tc.action, tc.rctx)
			if got != tc.wantAllow {
				t.Errorf("Can(%v, %v, %+v) = %v; want %v", tc.roles, tc.action, tc.rctx, got, tc.wantAllow)
			}
		})
	}
}
