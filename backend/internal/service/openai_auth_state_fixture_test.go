package service

import "reflect"

func matchesAuthFailureTestAccount(current, before *Account) bool {
	return current != nil && before != nil &&
		current.ID == before.ID && current.Status == before.Status && current.Schedulable == before.Schedulable &&
		reflect.DeepEqual(current.ProxyID, before.ProxyID) && reflect.DeepEqual(current.Credentials, before.Credentials)
}
