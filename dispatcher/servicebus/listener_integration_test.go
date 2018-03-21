package servicebus

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/lawrencegripper/mlops/dispatcher/types"
)

func prettyPrintStruct(item interface{}) string {
	b, _ := json.MarshalIndent(item, "", " ")
	return string(b)
}

// TestNewListener performs an end-2-end integration test on the listener talking to Azure ServiceBus
func TestNewListener(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Paniced: %v", prettyPrintStruct(r))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	listener := NewListener(ctx, types.Configuration{
		ClientID:            os.Getenv("AZURE_CLIENT_ID"),
		ClientSecret:        os.Getenv("AZURE_CLIENT_SECRET"),
		ResourceGroup:       os.Getenv("AZURE_RESOURCE_GROUP"),
		SubscriptionID:      os.Getenv("AZURE_SUBSCRIPTION_ID"),
		TenantID:            os.Getenv("AZURE_TENANT_ID"),
		ServiceBusNamespace: os.Getenv("AZURE_SERVICEBUS_NAMESPACE"),
		Hostname:            "Test",
		ModuleName:          "ModuleName",
		SubscribesToEvent:   "ExampleEvent",
		LogLevel:            "Debug",
	})

	//Listener will panic on error
	for {
		message := <-listener.ReceiveChannel
		t.Log("Received Message")
		t.Log(message)
		return
	}
}