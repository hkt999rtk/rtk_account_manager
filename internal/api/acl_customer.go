package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/auth"
	"rtk_account_manager/internal/store"
)

type customerRoleAssignmentRequest struct {
	RoleName  string  `json:"role_name" binding:"required"`
	ActorID   string  `json:"actor_id" binding:"required"`
	ScopeType string  `json:"scope_type" binding:"required"`
	ScopeID   *string `json:"scope_id"`
}

func (s *Server) checkOrganizationAccess(c *gin.Context) {
	if currentSubjectType(c) != auth.SubjectTypeUser {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	if _, err := s.store.GetRole(c.Request.Context(), c.Param("orgId"), currentUserID(c)); err != nil {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	permission := strings.TrimSpace(c.Query("permission"))
	scopeType := strings.TrimSpace(c.Query("scope_type"))
	scopeID := strings.TrimSpace(c.Query("scope_id"))
	if permission == "" || scopeType == "" || scopeID == "" {
		writeError(c, http.StatusBadRequest, "invalid_scope", "permission, scope_type, and scope_id are required")
		return
	}
	allowed, err := s.store.HasUserPermissionForResource(c.Request.Context(), currentUserID(c), c.Param("orgId"), permission, scopeType, scopeID)
	if err != nil {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"allowed": allowed, "scope_type": scopeType, "scope_id": scopeID})
}

func (s *Server) listCustomerACLRoles(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListRoles(c.Request.Context(), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": page.Roles, "pagination": page.Page})
}

func (s *Server) listCustomerACLPermissions(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListPermissions(c.Request.Context(), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"permissions": page.Permissions, "pagination": page.Page})
}

func (s *Server) listCustomerACLAssignments(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListRoleAssignmentsForOrganization(c.Request.Context(), c.Param("orgId"), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"role_assignments": page.Assignments, "pagination": page.Page})
}

func (s *Server) createCustomerACLAssignment(c *gin.Context) {
	var req customerRoleAssignmentRequest
	if !bind(c, &req) {
		return
	}
	orgID := strings.TrimSpace(c.Param("orgId"))
	actorID := strings.TrimSpace(req.ActorID)
	scopeType := strings.TrimSpace(req.ScopeType)
	if !requireNonBlank(c, "role_name", req.RoleName) || !requireNonBlank(c, "actor_id", actorID) || !requireNonBlank(c, "scope_type", scopeType) {
		return
	}
	if req.RoleName == store.ProductOwnerRole || req.RoleName == store.ProductEditorRole || req.RoleName == store.ProductViewerRole {
		writeError(c, http.StatusBadRequest, "managed_product_role", "Use the Product collaborator API to manage Product roles")
		return
	}
	if scopeType != store.ScopeTypeOrganization && scopeType != store.ScopeTypeProduct && scopeType != store.ScopeTypeRegion && scopeType != store.ScopeTypeGroup && scopeType != store.ScopeTypeDevice {
		writeError(c, http.StatusBadRequest, "invalid_scope_type", "Unsupported assignment scope")
		return
	}
	if scopeType == store.ScopeTypeOrganization {
		if req.ScopeID == nil || strings.TrimSpace(*req.ScopeID) != orgID {
			writeError(c, http.StatusBadRequest, "invalid_scope", "Organization scope must match the organization")
			return
		}
	} else if req.ScopeID == nil || strings.TrimSpace(*req.ScopeID) == "" {
		writeError(c, http.StatusBadRequest, "invalid_scope", "Resource scope requires a scope id")
		return
	}
	if _, err := s.store.GetRole(c.Request.Context(), orgID, actorID); err != nil {
		writeError(c, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	assignment, err := s.store.CreateRoleAssignment(c.Request.Context(), store.RoleAssignmentCreateInput{
		RoleName: req.RoleName, ActorType: store.ActorTypeUser, ActorID: actorID,
		ScopeType: scopeType, ScopeID: req.ScopeID, OrganizationID: stringPtr(orgID),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"role_assignment": assignment})
}

func (s *Server) deleteCustomerACLAssignment(c *gin.Context) {
	actor := currentUserID(c)
	var actorID *string
	if actor != "" {
		actorID = &actor
	}
	if err := s.store.DisableRoleAssignmentForOrganization(c.Request.Context(), c.Param("orgId"), c.Param("assignmentId"), actorID); err != nil {
		writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
