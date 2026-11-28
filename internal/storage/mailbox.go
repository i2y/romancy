package storage

import "strings"

// isOwnDirectChannel reports whether channel is instanceID's own direct
// channel "{name}:{instanceID}" (SEMANTICS §3). A direct channel is a
// mailbox: a subscription to it also receives the messages sent before it.
func isOwnDirectChannel(instanceID, channel string) bool {
	return instanceID != "" && len(channel) > len(instanceID)+1 &&
		strings.HasSuffix(channel, ":"+instanceID)
}

// waitChannels lists the channels one receive waits on: channelName first,
// then the others, without repeats.
func waitChannels(channelName string, also []string) []string {
	channels := []string{channelName}
	for _, c := range also {
		if c != "" && !contains(channels, c) {
			channels = append(channels, c)
		}
	}
	return channels
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
