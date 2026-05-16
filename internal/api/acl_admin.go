package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"rtk_account_manager/internal/store"
)

type createACLRoleRequest struct {
	Name        string  `json:"name" binding:"required"`
	ScopeType   string  `json:"scope_type" binding:"required"`
	Description *string `json:"description"`
}

type updateACLRoleRequest struct {
	Description *string `json:"description"`
}

type createACLRoleAssignmentRequest struct {
	RoleName       string  `json:"role_name" binding:"required"`
	ActorType      string  `json:"actor_type"`
	ActorID        string  `json:"actor_id" binding:"required"`
	ScopeType      string  `json:"scope_type" binding:"required"`
	ScopeID        *string `json:"scope_id"`
	OrganizationID *string `json:"organization_id"`
}

type createACLExternalGroupMappingRequest struct {
	ProviderID     string  `json:"provider_id" binding:"required"`
	ExternalGroup  string  `json:"external_group" binding:"required"`
	RoleName       string  `json:"role_name" binding:"required"`
	ScopeType      string  `json:"scope_type" binding:"required"`
	ScopeID        *string `json:"scope_id"`
	OrganizationID *string `json:"organization_id"`
}

func (s *Server) listACLPermissions(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListPermissions(c.Request.Context(), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"permissions": page.Permissions, "pagination": page.Page})
}

func (s *Server) listACLRoles(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListRoles(c.Request.Context(), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": page.Roles, "pagination": page.Page})
}

func (s *Server) createACLRole(c *gin.Context) {
	var req createACLRoleRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "name", req.Name) || !requireNonBlank(c, "scope_type", req.ScopeType) {
		return
	}
	role, err := s.store.CreateRole(c.Request.Context(), store.RoleCreateInput{
		Name:        strings.TrimSpace(req.Name),
		ScopeType:   strings.TrimSpace(req.ScopeType),
		Description: req.Description,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"role": role})
}

func (s *Server) getACLRole(c *gin.Context) {
	role, err := s.store.GetRoleByName(c.Request.Context(), strings.TrimSpace(c.Param("roleName")))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"role": role})
}

func (s *Server) updateACLRole(c *gin.Context) {
	var req updateACLRoleRequest
	if !bind(c, &req) {
		return
	}
	roleName := strings.TrimSpace(c.Param("roleName"))
	if !requireNonBlank(c, "role_name", roleName) {
		return
	}
	actor := currentUserID(c)
	role, err := s.store.UpdateRole(c.Request.Context(), store.RoleUpdateInput{
		Name:        roleName,
		Description: req.Description,
		ActorUserID: &actor,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"role": role})
}

func (s *Server) deleteACLRole(c *gin.Context) {
	roleName := strings.TrimSpace(c.Param("roleName"))
	if !requireNonBlank(c, "role_name", roleName) {
		return
	}
	actor := currentUserID(c)
	if err := s.store.DisableRole(c.Request.Context(), roleName, &actor); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) bindACLRolePermission(c *gin.Context) {
	roleName := strings.TrimSpace(c.Param("roleName"))
	permissionName := strings.TrimSpace(c.Param("permissionName"))
	if !requireNonBlank(c, "role_name", roleName) || !requireNonBlank(c, "permission_name", permissionName) {
		return
	}
	actor := currentUserID(c)
	if err := s.store.BindRolePermission(c.Request.Context(), roleName, permissionName, &actor); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) createACLRoleAssignment(c *gin.Context) {
	var req createACLRoleAssignmentRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "role_name", req.RoleName) ||
		!requireNonBlank(c, "actor_id", req.ActorID) ||
		!requireNonBlank(c, "scope_type", req.ScopeType) {
		return
	}
	actor := currentUserID(c)
	assignment, err := s.store.CreateRoleAssignment(c.Request.Context(), store.RoleAssignmentCreateInput{
		RoleName:       strings.TrimSpace(req.RoleName),
		ActorType:      strings.TrimSpace(req.ActorType),
		ActorID:        strings.TrimSpace(req.ActorID),
		ScopeType:      strings.TrimSpace(req.ScopeType),
		ScopeID:        trimStringPtr(req.ScopeID),
		OrganizationID: trimStringPtr(req.OrganizationID),
		CreatedBy:      &actor,
		Now:            s.now(),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"role_assignment": assignment})
}

func (s *Server) listACLRoleAssignments(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListRoleAssignments(c.Request.Context(), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"role_assignments": page.Assignments, "pagination": page.Page})
}

func (s *Server) deleteACLRoleAssignment(c *gin.Context) {
	assignmentID := strings.TrimSpace(c.Param("assignmentId"))
	if !requireNonBlank(c, "assignment_id", assignmentID) {
		return
	}
	actor := currentUserID(c)
	if err := s.store.DisableRoleAssignment(c.Request.Context(), assignmentID, &actor); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) createACLExternalGroupMapping(c *gin.Context) {
	var req createACLExternalGroupMappingRequest
	if !bind(c, &req) {
		return
	}
	if !requireNonBlank(c, "provider_id", req.ProviderID) ||
		!requireNonBlank(c, "external_group", req.ExternalGroup) ||
		!requireNonBlank(c, "role_name", req.RoleName) ||
		!requireNonBlank(c, "scope_type", req.ScopeType) {
		return
	}
	actor := currentUserID(c)
	mapping, err := s.store.CreateExternalGroupMapping(c.Request.Context(), store.ExternalGroupMappingCreateInput{
		ProviderID:     strings.TrimSpace(req.ProviderID),
		ExternalGroup:  strings.TrimSpace(req.ExternalGroup),
		RoleName:       strings.TrimSpace(req.RoleName),
		ScopeType:      strings.TrimSpace(req.ScopeType),
		ScopeID:        trimStringPtr(req.ScopeID),
		OrganizationID: trimStringPtr(req.OrganizationID),
		CreatedBy:      &actor,
		Now:            s.now(),
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"external_group_mapping": mapping})
}

func (s *Server) listACLExternalGroupMappings(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListExternalGroupMappings(c.Request.Context(), limit, offset)
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"external_group_mappings": page.Mappings, "pagination": page.Page})
}

func (s *Server) deleteACLExternalGroupMapping(c *gin.Context) {
	mappingID := strings.TrimSpace(c.Param("mappingId"))
	if !requireNonBlank(c, "mapping_id", mappingID) {
		return
	}
	actor := currentUserID(c)
	if err := s.store.DisableExternalGroupMapping(c.Request.Context(), mappingID, &actor); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) listACLAuditEvents(c *gin.Context) {
	limit, offset := pagination(c)
	page, err := s.store.ListACLAuditEvents(c.Request.Context(), store.ACLAuditEventListFilter{
		EventType:      strings.TrimSpace(c.Query("event_type")),
		SubjectType:    strings.TrimSpace(c.Query("subject_type")),
		OrganizationID: strings.TrimSpace(c.Query("organization_id")),
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"audit_events": page.Events, "pagination": page.Page})
}

func trimStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
