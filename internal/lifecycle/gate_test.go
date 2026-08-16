package lifecycle

import "testing"

func TestGateMakesUpdatesAndOperationsMutuallyExclusive(t *testing.T) {
	gate := New()
	first, ok := gate.TryOperation()
	if !ok {
		t.Fatal("first operation was rejected")
	}
	second, ok := gate.TryOperation()
	if !ok {
		t.Fatal("concurrent operation was rejected")
	}
	if update, ok := gate.TryUpdate(); ok || update != nil {
		t.Fatal("update acquired while operations were active")
	}
	first.Release()
	first.Release()
	if update, ok := gate.TryUpdate(); ok || update != nil {
		t.Fatal("update acquired before every operation completed")
	}
	second.Release()

	update, ok := gate.TryUpdate()
	if !ok {
		t.Fatal("update was rejected after operations completed")
	}
	if operation, ok := gate.TryOperation(); ok || operation != nil {
		t.Fatal("operation acquired during update")
	}
	update.Release()
	if operation, ok := gate.TryOperation(); !ok || operation == nil {
		t.Fatal("operation was rejected after update completed")
	} else {
		operation.Release()
	}
}
