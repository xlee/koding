package main

import (
	"encoding/json"
	"fmt"
	"github.com/siesta/neo4j"
	"github.com/streadway/amqp"
	"io/ioutil"
	oldNeo "koding/databases/neo4j"
	"koding/db/mongodb"
	"koding/tools/amqputil"
	"koding/tools/config"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type strToInf map[string]interface{}

var (
	GRAPH_URL           = config.Current.Neo4j.Write + ":" + strconv.Itoa(config.Current.Neo4j.Port)
	EXCHANGENAME        = "graphFeederExchange"
	MAX_ITERATION_COUNT = 50
	GUEST_GROUP_ID      = "51f41f195f07655e560001c1"
)

func main() {
	log.Println("Sync worker started")

	amqpChannel := connectToRabbitMQ()
	log.Println("Connected to Rabbit")

	mongo := mongodb.NewMongoDB(config.Current.Mongo)
	coll := mongo.GetSession().DB("").C("relationships")
	filter := strToInf{
		"targetName": strToInf{"$nin": oldNeo.NotAllowedNames},
		"sourceName": strToInf{"$nin": oldNeo.NotAllowedNames},
	}
	query := coll.Find(filter)
	totalCount, err := query.Count()
	if err != nil {
		log.Println("Err while getting count, exiting", err)
		return
	}

	skip := config.Skip
	// this is a starting point
	index := skip
	// this is the item count to be processed
	limit := config.Count
	// this will be the ending point
	count := index + limit

	var result oldNeo.Relationship

	iteration := 0
	for {
		// if we reach to the end of the all collection, exit
		if index == totalCount {
			log.Println("All items are processed, exiting")
			break
		}

		// this is the max re-iterating count
		if iteration == MAX_ITERATION_COUNT {
			break
		}

		// if we processed all items then exit
		if index == count {
			break
		}

		iter := query.Skip(index).Limit(count - index).Iter()
		for iter.Next(&result) {
			time.Sleep(100 * time.Millisecond)

			if relationshipNeedsToBeSynced(result) {
				createRelationship(result, amqpChannel)
			}

			index++
			log.Println(index)
		}

		if err := iter.Close(); err != nil {
			log.Println(err)
		}

		if iter.Timeout() {
			continue
		}

		log.Printf("iter existed, starting over from %v  -- %v  item(s) are processsed on this iter", index+1, index-skip)
		iteration++
	}

	if iteration == MAX_ITERATION_COUNT {
		log.Printf("Max iteration count %v reached, exiting", iteration)
	}
	log.Printf("Synced %v entries on this process", index-skip)
}

func connectToRabbitMQ() *amqp.Channel {
	conn := amqputil.CreateConnection("syncWorker")
	amqpChannel, err := conn.Channel()
	if err != nil {
		panic(err)
	}
	return amqpChannel
}

func createRelationship(rel oldNeo.Relationship, amqpChannel *amqp.Channel) {
	data := make([]strToInf, 1)
	data[0] = strToInf{
		"_id":        rel.Id,
		"sourceId":   rel.SourceId,
		"sourceName": rel.SourceName,
		"targetId":   rel.TargetId,
		"targetName": rel.TargetName,
		"as":         rel.As,
	}

	eventData := strToInf{"event": "RelationshipSaved", "payload": data}

	neoMessage, err := json.Marshal(eventData)
	if err != nil {
		log.Println("unmarshall error")
		return
	}

	amqpChannel.Publish(
		EXCHANGENAME, // exchange name
		"",           // key
		false,        // mandatory
		false,        // immediate
		amqp.Publishing{
			Body: neoMessage,
		},
	)
}

func relationshipNeedsToBeSynced(result oldNeo.Relationship) bool {
	if result.SourceId.Hex() == GUEST_GROUP_ID || result.TargetId.Hex() == GUEST_GROUP_ID {
		return false
	}

	exists, sourceId := checkNodeExists(result.SourceId.Hex())
	if exists != true {
		logError(result, "No source node")
		return true
	}

	exists, targetId := checkNodeExists(result.TargetId.Hex())
	if exists != true {
		logError(result, "No target node")
		return true
	}

	// flip JGroup relationships since they take a long time to check
	var flipped = false
	if result.SourceName == "JGroup" {
		flipped = true

		tempId := sourceId
		sourceId = targetId
		targetId = tempId
	}

	exists = checkRelationshipExists(sourceId, targetId, result.As, flipped)
	if exists != true {
		logError(result, "No relationship")
		return true
	}

	// everything is fine
	return false
}

func getAndParse(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func checkRelationshipExists(sourceId, targetId, relType string, flipped bool) bool {
	url := fmt.Sprintf("%v/db/data/node/%v/relationships/all/%v", GRAPH_URL, sourceId, relType)

	body, err := getAndParse(url)
	if err != nil {
		return false
	}

	relResponse := make([]neo4j.RelationshipResponse, 1)
	err = json.Unmarshal(body, &relResponse)
	if err != nil {
		return false
	}

	var numberofRelsFound = 0
	var relIds []string

	for _, rl := range relResponse {
		var checkPos string
		if flipped {
			checkPos = rl.Start
		} else {
			checkPos = rl.End
		}

		id := strings.SplitAfter(checkPos, GRAPH_URL+"/db/data/node/")[1]
		if targetId == id {
			numberofRelsFound++
			relIds = append(relIds, getRelIdFromUrl(rl.Self))
		}
	}

	switch numberofRelsFound {
	case 0:
		return false
	case 1:
		return true
	default:
		log.Printf("multiple '%v' rel %v", relType, relIds)

		if relType == "member" || relType == "creator" || relType == "author" {
			deleteDuplicateRel(relIds[1:])
			log.Printf("deleted multiple '%v' rel", relType)
		}
	}

	return true
}

func deleteDuplicateRel(relIds []string) {
	for _, id := range relIds {
		rel := neo4j.Relationship{Id: id}

		neo4jConnection := neo4j.Connect(GRAPH_URL)
		batch := neo4jConnection.NewBatch().Delete(&rel)

		_, err := batch.Execute()
		if err != nil {
			log.Println("err deleting rel", err)
		}
	}
}

func checkNodeExists(id string) (bool, string) {
	url := fmt.Sprintf("%v/db/data/index/node/koding/id/%v", GRAPH_URL, id)
	body, err := getAndParse(url)
	if err != nil {
		return false, ""
	}

	nodeResponse := make([]neo4j.NodeResponse, 1)
	err = json.Unmarshal(body, &nodeResponse)
	if err != nil {
		return false, ""
	}

	if len(nodeResponse) < 1 {
		return false, ""
	}

	node := nodeResponse[0]
	idd := getNodeIdFromUrl(node.Self)

	nodeId := string(idd)
	if nodeId == "" {
		return false, ""
	}

	return true, nodeId
}

func getNodeIdFromUrl(nodeSelf string) string {
	return strings.SplitAfter(nodeSelf, GRAPH_URL+"/db/data/node/")[1]
}

func getRelIdFromUrl(nodeSelf string) string {
	return strings.SplitAfter(nodeSelf, GRAPH_URL+"/db/data/relationship/")[1]
}

func logError(result oldNeo.Relationship, errMsg string) {
	log.Printf("id: %v, type: %v, source: {%v %v} target: {%v %v}; err: %v", result.SourceId.Hex(), result.As, result.SourceId.Hex(), result.SourceName, result.TargetId.Hex(), result.TargetName, errMsg)
}
