package models

import (
	"time"
	"gopkg.in/mgo.v2/bson"
	"github.com/gin-gonic/gin"
)


// Organization is the model for organization
// collection
type Group struct {
	ID                       bson.ObjectId  `bson:"_id" json:"id"`

	// required feilds
	Name                     string         `bson:"name" json:"name" binding:"required"`

	Description              *string         `bson:"description,omitempty" json:"description"`
	Variables                *string         `bson:"variables,omitempty" json:"variables"`
	TotalHosts               *uint32         `bson:"total_hosts,omitempty" json:"total_hosts"`
	HasActiveFailures        bool           `bson:"has_active_failures,omitempty" json:"has_active_failures"`
	HostsWithActiveFailures  *uint32         `bson:"hosts_with_active_failures,omitempty" json:"hosts_with_active_failures"`
	TotalGroups              *uint32         `bson:"total_groups,omitempty" json:"total_groups"`
	GroupsWithActiveFailures *uint32         `bson:"groups_with_active_failures,omitempty" json:"groups_with_active_failures"`
	HasInventorySources      bool           `bson:"has_inventory_sources,omitempty" json:"has_inventory_sources"`
	InventoryID              bson.ObjectId  `bson:"inventory_id,omitempty" json:"inventory"`
	//parent child relation
	ParentGroupID            *bson.ObjectId  `bson:"parent_group_id,omitempty" json:"-"`

	CreatedByID              bson.ObjectId  `bson:"created_by_id" json:"-"`
	ModifiedByID             bson.ObjectId  `bson:"modified_by_id" json:"-"`

	Created                  time.Time      `bson:"created" json:"created"`
	Modified                 time.Time      `bson:"modified" json:"modified"`

	Type                     string         `bson:"-" json:"type"`
	Url                      string         `bson:"-" json:"url"`
	Related                  gin.H          `bson:"-" json:"related"`
	Summary                  gin.H          `bson:"-" json:"summary_fields"`
	LastJob                  gin.H          `bson:"-" json:"last_job"`
	LastJobHostSummary       gin.H          `bson:"-" json:"last_job_host_summary"`
}