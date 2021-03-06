// Copyright (C) 2020 David Tagatac <david@tagatac.net>
// See main.go for usage terms.

// Package chatdb provides an interface ChatDB for interacting with the Mac OS
// Messages database typically located at $HOME/Library/Messages/chat.db. See
// [this Medium post](https://towardsdatascience.com/heres-how-you-can-access-your-entire-imessage-history-on-your-mac-f8878276c6e9)
// for a decent primer on navigating the database. Specifically, this package is
// tailored to supporting the exporting of all messages in the database to
// readable, searchable text files.
package chatdb

import (
	"database/sql"
	"fmt"

	"github.com/Masterminds/semver"
	"github.com/emersion/go-vcard"
	"github.com/pkg/errors"
)

const _githubIssueMsg = "open an issue at https://github.com/tagatac/bagoup/issues"

// Adapted from https://apple.stackexchange.com/a/300997/267331
const (
	_datetimeFormulaLegacy = "date + STRFTIME('%s', '2001-01-01 00:00:00'), 'unixepoch', 'localtime'"
	_datetimeFormula       = "(date/1000000000) + STRFTIME('%s', '2001-01-01 00:00:00'), 'unixepoch', 'localtime'"
)

var _modernVersion = semver.MustParse("10.13")

// Chat represents a row from the chat table.
type Chat struct {
	ID          int
	GUID        string
	DisplayName string
}

//go:generate mockgen -destination=mock_chatdb/mock_chatdb.go github.com/tagatac/bagoup/chatdb ChatDB

type (
	// ChatDB extracts data from a Mac OS Messages database on disk.
	ChatDB interface {
		// GetHandleMap returns a mapping from handle ID to phone number or email
		// address. If a contact map is supplied, it will attempt to resolve these
		// handles to formatted names.
		GetHandleMap(contactMap map[string]*vcard.Card) (map[int]string, error)
		// GetChats returns a slice of Chat, effectively a table scan of the chat
		// table.
		GetChats(contactMap map[string]*vcard.Card) ([]Chat, error)
		// GetMessageIDs returns a slice of message IDs corresponding to a given
		// chat ID, in the order that the messages are timestamped.
		GetMessageIDs(chatID int) ([]int, error)
		// GetMessage returns a message retrieved from the database formatted for
		// writing to a chat file.
		GetMessage(messageID int, handleMap map[int]string, macOSVersion *semver.Version) (string, error)
	}

	chatDB struct {
		*sql.DB
		datetimeFormula string
		selfHandle      string
	}
)

// NewChatDB returns a ChatDB interface using the given DB.
func NewChatDB(db *sql.DB, selfHandle string) ChatDB {
	return &chatDB{
		DB:         db,
		selfHandle: selfHandle,
	}
}

func (d chatDB) GetHandleMap(contactMap map[string]*vcard.Card) (map[int]string, error) {
	handleMap := make(map[int]string)
	handles, err := d.DB.Query("SELECT ROWID, id FROM handle")
	if err != nil {
		return nil, errors.Wrap(err, "get handles from DB")
	}
	defer handles.Close()
	for handles.Next() {
		var handleID int
		var handle string
		if err := handles.Scan(&handleID, &handle); err != nil {
			return nil, errors.Wrap(err, "read handle")
		}
		if _, ok := handleMap[handleID]; ok {
			return nil, fmt.Errorf("multiple handles with the same ID: %d - handle ID uniqueness assumption violated - %s", handleID, _githubIssueMsg)
		}
		if card, ok := contactMap[handle]; ok {
			name := card.Name()
			if name != nil && name.GivenName != "" {
				handle = name.GivenName
			}
		}
		handleMap[handleID] = handle
	}
	return handleMap, nil
}

func (d chatDB) GetChats(contactMap map[string]*vcard.Card) ([]Chat, error) {
	chatRows, err := d.DB.Query("SELECT ROWID, guid, chat_identifier, COALESCE(display_name, '') FROM chat")
	if err != nil {
		return nil, errors.Wrap(err, "query chats table")
	}
	defer chatRows.Close()
	chats := []Chat{}
	for chatRows.Next() {
		var id int
		var guid, name, displayName string
		if err := chatRows.Scan(&id, &guid, &name, &displayName); err != nil {
			return nil, errors.Wrap(err, "read chat")
		}
		if displayName == "" {
			displayName = name
		}
		if card, ok := contactMap[displayName]; ok {
			contactName := card.PreferredValue(vcard.FieldFormattedName)
			if contactName != "" {
				displayName = contactName
			}
		}
		chats = append(chats, Chat{
			ID:          id,
			GUID:        guid,
			DisplayName: displayName,
		})
	}
	return chats, nil
}

func (d chatDB) GetMessageIDs(chatID int) ([]int, error) {
	rows, err := d.DB.Query(fmt.Sprintf("SELECT message_id FROM chat_message_join WHERE chat_id=%d", chatID))
	if err != nil {
		return nil, errors.Wrapf(err, "query chat_message_join table for chat ID %d", chatID)
	}
	defer rows.Close()
	messageIDs := []int{}
	for rows.Next() {
		var messageID int
		if err := rows.Scan(&messageID); err != nil {
			return nil, errors.Wrapf(err, "read message ID for chat ID %d", chatID)
		}
		messageIDs = append(messageIDs, messageID)
	}
	return messageIDs, nil
}

func (d *chatDB) GetMessage(messageID int, handleMap map[int]string, macOSVersion *semver.Version) (string, error) {
	messages, err := d.DB.Query(fmt.Sprintf("SELECT is_from_me, handle_id, COALESCE(text, ''), DATETIME(%s) FROM message WHERE ROWID=%d", d.getDatetimeFormula(macOSVersion), messageID))
	if err != nil {
		return "", errors.Wrapf(err, "query message table for ID %d", messageID)
	}
	defer messages.Close()
	messages.Next()
	var fromMe, handleID int
	var text, date string
	if err := messages.Scan(&fromMe, &handleID, &text, &date); err != nil {
		return "", errors.Wrapf(err, "read data for message ID %d", messageID)
	}
	if messages.Next() {
		return "", fmt.Errorf("multiple messages with the same ID: %d - message ID uniqeness assumption violated - %s", messageID, _githubIssueMsg)
	}
	handle := handleMap[handleID]
	if fromMe == 1 {
		handle = d.selfHandle
	}
	return fmt.Sprintf("[%s] %s: %s\n", date, handle, text), nil
}

func (d *chatDB) getDatetimeFormula(macOSVersion *semver.Version) string {
	if d.datetimeFormula != "" {
		return d.datetimeFormula
	}
	if macOSVersion != nil && macOSVersion.LessThan(_modernVersion) {
		return _datetimeFormulaLegacy
	}
	return _datetimeFormula
}
