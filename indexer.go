package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const stashIndexFile = "stash_index.dat"
const latestIdFile = "latest_id"

type Indexer struct {
	currentID  string
	stashItems map[string]map[string]bool
	filterFunc func(*Item) bool
	itemCh     chan *itemBatch

	parseCh chan struct{}
	indexCh chan struct{}
	doneCh  chan struct{}
	resetCh chan string
}

type itemBatch struct {
	items     []*Item
	deletions map[string]int64
	apiID     string
}

func NewIndexer() (*Indexer, error) {
	i := &Indexer{
		filterFunc: standardPriceFilter,
		itemCh:     make(chan *itemBatch, 1024),
		parseCh:    make(chan struct{}, 0),
		indexCh:    make(chan struct{}, 0),
		doneCh:     make(chan struct{}, 0),
		resetCh:    make(chan string, 1),
	}

	file, err := os.Open(stashIndexFile)
	if err != nil {
		log.Printf("Error: %v", err)
		i.stashItems = make(map[string]map[string]bool)
	} else {
		decoder := gob.NewDecoder(file)
		decoder.Decode(&i.stashItems)
	}

	bytes, err := ioutil.ReadFile(latestIdFile)
	if err != nil {
		log.Printf("error opening id file: %s", err)
	} else {
		i.currentID = string(bytes)
	}

	return i, nil
}

func (i *Indexer) start() {
	go i.parseUpdates()
	go i.indexLoop()
}

func (i *Indexer) shutdown() {
	i.parseCh <- struct{}{}
	i.indexCh <- struct{}{}
}

func (i *Indexer) parseUpdates() {
	client := new(http.Client)
	totalParsed := 0

	defer func() {
		i.persistStashIndex()
		i.doneCh <- struct{}{}
	}()

	for {
		select {
		case <-i.parseCh:
			log.Println("parser stopped")
			return
		case id := <-i.resetCh:
			i.currentID = id
			log.Printf("Retrying ID %q", id)
		default:
		}

		req, err := http.NewRequest("GET", "http://api.pathofexile.com/public-stash-tabs?id="+i.currentID, nil)
		if err != nil {
			log.Printf("error creating request: %v", err)
			continue
		}

		start := time.Now()
		response, err := client.Do(req)
		if err != nil {
			log.Printf("error getting request: %v", err)
			continue
		}
		defer response.Body.Close()
		bytes, err := ioutil.ReadAll(response.Body)
		if err != nil {
			log.Printf("error reading response body: %v", err)
			continue
		}

		var stashes StashTabResponse
		err = json.Unmarshal(bytes, &stashes)
		if err != nil {
			log.Printf("error parsing json: %v", err)
			log.Printf("len: %v", len(bytes))
			continue
		}
		stashes.ID = i.currentID

		if stashes.NextChangeID != i.currentID {
			totalParsed += i.ingestResponse(&stashes)
			log.Printf("total parsed: %d", totalParsed)
			log.Printf("parsed stash page: %q", stashes.ID)
		} else {
			log.Println("Reached the end of the stream, waiting 1s for updates...")
			time.Sleep(1 * time.Second)
		}

		// Sleep so we don't request too frequently
		end := time.Now()
		diff := end.Sub(start)
		if diff < time.Second {
			time.Sleep(time.Second - diff)
		}

		i.currentID = stashes.NextChangeID
	}
}

func (i *Indexer) ingestResponse(tabs *StashTabResponse) int {
	var selected []*Item
	deletions := make(map[string]int64)

	for _, stash := range tabs.Stashes {
		tabItems := make(map[string]bool)
		//log.Printf("stash tab ID: %q, items: %d", stash.ID, len(stash.Items))

		for _, item := range stash.Items {
			if strings.HasPrefix(item.Note, "~price") || strings.HasPrefix(item.Note, "~b") {
				item.Price = item.Note
			} else if strings.HasPrefix(stash.Stash, "~price") || strings.HasPrefix(stash.Stash, "~b") {
				item.Price = stash.Stash
			}

			if i.filterFunc(item) {
				item.LastUpdated = time.Now().Unix()
				if _, ok := i.stashItems[stash.ID]; !ok {
					item.Created = item.LastUpdated
				} else if _, ok := i.stashItems[stash.ID][item.ID]; !ok {
					item.Created = item.LastUpdated
				}
				item.Name = strings.TrimPrefix(item.Name, "<<set:MS>><<set:M>><<set:S>>")
				item.Account = stash.AccountName
				item.StashID = stash.ID

				tabItems[item.ID] = true
				selected = append(selected, item)
			}
		}

		// Check if any items have been removed from this tab
		if oldTabItems, ok := i.stashItems[stash.ID]; ok {
			for id, _ := range oldTabItems {
				if _, ok := tabItems[id]; !ok {
					deletions[id] = time.Now().Unix()
					log.Printf("Item removed: %q", id)
				}
			}

			// remove the tab if it's now empty
			if len(tabItems) == 0 {
				delete(i.stashItems, stash.ID)
				log.Printf("Deleting empty tab: %q", stash.ID)
			}
		}

		if len(tabItems) >= 1 {
			i.stashItems[stash.ID] = tabItems
		}
	}

	i.itemCh <- &itemBatch{
		items:     selected,
		deletions: deletions,
		apiID:     tabs.ID,
	}

	return len(selected)
}

func (i *Indexer) indexLoop() {
	totalIndexed := 0
	for {
		select {
		case <-i.indexCh:
			log.Println("indexer stopped")
			return
		case batch := <-i.itemCh:
			err := i.indexBatch(batch)
			if err != nil {
				log.Printf("Index error: %v", err)
				i.resetCh <- batch.apiID
			} else {
				ioutil.WriteFile(latestIdFile, []byte(batch.apiID), 0644)
				err = i.persistLatestID(batch.apiID)
				if err != nil {
					log.Printf("Error persisting ID %q: %v", batch.apiID, err)
				}

				totalIndexed += len(batch.items)
				log.Printf("total indexed: %d", totalIndexed)
			}
		}
	}
}

func (i *Indexer) indexBatch(batch *itemBatch) error {
	if len(batch.items) == 0 && len(batch.deletions) == 0 {
		return nil
	}

	body := &bytes.Buffer{}

	for _, item := range batch.items {
		json, _ := json.Marshal(item)

		body.WriteString(fmt.Sprintf(`{"update":{"_id":"%s"}}`+"\n", item.ID))
		body.WriteString(`{"doc_as_upsert":true,"doc":`)
		body.Write(json)
		body.WriteString("}\n")
	}

	for id, removeDate := range batch.deletions {
		body.WriteString(fmt.Sprintf(`{"update":{"_id":"%s"}}`+"\n", id))
		body.WriteString(fmt.Sprintf(`{"doc_as_upsert":true,"doc":{"removed":%d}}`+"\n", removeDate))
	}

	req, err := http.NewRequest("POST", "http://linux-server:9200/items/item/_bulk", body)
	if err != nil {
		return err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		log.Printf("Error: status code %d", resp.StatusCode)
		log.Printf("Headers: %v", resp.Header)
		body, _ := ioutil.ReadAll(resp.Body)
		log.Println("response Body:", string(body))
		return fmt.Errorf("Unexpected status code %d", resp.StatusCode)
	}

	return nil
}

func (i *Indexer) persistStashIndex() error {
	file, err := os.Create(stashIndexFile)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := gob.NewEncoder(file)
	encoder.Encode(i.stashItems)

	return nil
}

func (i *Indexer) persistLatestID(id string) error {
	body := fmt.Sprintf(`{"latest_id":"%s"}`, id)
	return doRequest(&http.Client{}, "PUT", "linux-server:9200/meta/info/1", bytes.NewBufferString(body), nil)
}