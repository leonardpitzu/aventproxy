package tuya

import (
	"sync"
	"testing"
)

// TestCameraClientMapIsRaceFree drives the session map from several goroutines
// at once. The broker delivers messages on its own goroutine, so lookups run
// concurrently with streams starting and stopping; without a lock this is a
// concurrent map read and map write, which takes the whole bridge down.
func TestCameraClientMapIsRaceFree(t *testing.T) {
	c := &MQTTClient{}

	const sessions = 8
	var wg sync.WaitGroup

	for i := range sessions {
		wg.Add(2)

		id := string(rune('a' + i))
		go func() {
			defer wg.Done()
			for range 200 {
				c.AddCameraClient(id, &MQTTCameraClient{SessionId: id})
				c.RemoveCameraClient(id)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				c.cameraClient(id)
			}
		}()
	}

	wg.Wait()
}

func TestRemoveCameraClientOnEmptyMapIsSafe(t *testing.T) {
	c := &MQTTClient{}
	c.RemoveCameraClient("never-added")

	if _, ok := c.cameraClient("never-added"); ok {
		t.Error("lookup of a session that was never added should miss")
	}
}
