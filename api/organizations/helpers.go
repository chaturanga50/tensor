package organizations

import (
	"bitbucket.pearson.com/apseng/tensor/db"
	"bitbucket.pearson.com/apseng/tensor/models"
	log "github.com/Sirupsen/logrus"
	"gopkg.in/mgo.v2/bson"
	"time"
)

func addActivity(crdID bson.ObjectId, userID bson.ObjectId, desc string) {

	a := models.Activity{
		ID:          bson.NewObjectId(),
		ActorID:     userID,
		Type:        _CTX_ORGANIZATION,
		ObjectID:    crdID,
		Description: desc,
		Created:     time.Now(),
	}

	if err := db.ActivityStream().Insert(a); err != nil {
		log.Errorln("Failed to add new Activity", err)
	}
}