package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.google.com/p/go.crypto/curve25519"
	"code.google.com/p/goprotobuf/proto"
	"github.com/agl/ed25519"
	"github.com/agl/pond/bbssig"
	pond "github.com/agl/pond/protos"
)

// messageLifetime is the default amount of time for which we'll keep a
// message. (Counting from the time that it was received.)
const messageLifetime = 7 * 24 * time.Hour

const (
	colorWhite                 = 0xffffff
	colorGray                  = 0xfafafa
	colorHighlight             = 0xffebcd
	colorSubline               = 0x999999
	colorHeaderBackground      = 0xececed
	colorHeaderForeground      = 0x777777
	colorHeaderForegroundSmall = 0x7b7f83
	colorSep                   = 0xc9c9c9
	colorTitleForeground       = 0xdddddd
	colorBlack                 = 1
	colorRed                   = 0xff0000
	colorError                 = 0xff0000
)

const (
	fontLoadTitle   = "DejaVu Serif 30"
	fontLoadLarge   = "Arial Bold 30"
	fontListHeading = "Ariel Bold 11"
	fontListEntry   = "Liberation Sans 12"
	fontListSubline = "Liberation Sans 10"
	fontMainTitle   = "Arial 16"
	fontMainLabel   = "Arial Bold 9"
	fontMainBody    = "Arial 12"
	fontMainMono    = "Liberation Mono 10"
)

const (
	uiStateInvalid = iota
	uiStateLoading
	uiStateError
	uiStateMain
	uiStateCreateAccount
	uiStateCreatePassphrase
	uiStateNewContact
	uiStateNewContact2
	uiStateShowContact
	uiStateCompose
	uiStateOutbox
	uiStateShowIdentity
	uiStatePassphrase
	uiStateInbox
)

const shortTimeFormat = "Jan _2 15:04"
const logTimeFormat = "Jan _2 15:04:05"
const keyExchangePEM = "POND KEY EXCHANGE"

// client is the main structure containing most of the client's state.
type client struct {
	// testing is true in unittests and disables some assertions that are
	// needed in the real world, but which make testing difficult.
	testing bool
	// autoFetch controls whether the network goroutine performs periodic
	// transactions or waits for outside prompting.
	autoFetch bool

	// stateFilename is the filename of the file on disk in which we
	// load/save our state.
	stateFilename string
	// diskSalt contains the scrypt salt used to derive the state
	// encryption key.
	diskSalt [sCryptSaltLen]byte
	// diskKey is the XSalsa20 key used to encrypt the disk state.
	diskKey [32]byte

	ui UI
	// server is the URL of the user's home server.
	server string
	// identity is a curve25519 private value that's used to authenticate
	// the client to its home server.
	identity, identityPublic [32]byte
	// groupPriv is the group private key for the user's delivery group.
	groupPriv *bbssig.PrivateKey
	// generation is the generation number of the group private key and is
	// incremented when a member of the group is revoked.
	generation uint32
	// priv is an Ed25519 private key.
	priv [64]byte
	// pub is the public key corresponding to |priv|.
	pub  [32]byte
	rand io.Reader
	// writerChan is a channel that the disk goroutine reads from to
	// receive updated, serialised states.
	writerChan chan []byte
	// writerDone is a channel that is closed by the disk goroutine when it
	// has finished all pending updates.
	writerDone chan bool
	// fetchNowChan is the channel that the network goroutine reads from
	// that triggers an immediate network transaction. Mostly intended for
	// testing.
	fetchNowChan chan chan bool

	log *Log

	inboxUI, outboxUI, contactsUI, clientUI *listUI
	outbox                                  []*queuedMessage
	contacts                                map[uint64]*Contact
	inbox                                   []*InboxMessage

	// queue is a queue of messages for transmission that's shared with the
	// network goroutine and protected by queueMutex.
	queue      []*queuedMessage
	queueMutex sync.Mutex
	// newMessageChan receives messages that have been read from the home
	// server by the network goroutine.
	newMessageChan chan NewMessage
	// messageSentChan receives the ids of messages that have been sent by
	// the network goroutine.
	messageSentChan chan uint64
}

// InboxMessage represents a message in the client's inbox. (Although acks also
// appear as InboxMessages, but their message.Body is empty.)
type InboxMessage struct {
	id           uint64
	read         bool
	receivedTime time.Time
	from         uint64
	// sealed contained the encrypted message if the contact who sent this
	// message is still pending.
	sealed []byte
	acked  bool
	// message may be nil if the contact who sent this is pending. In this
	// case, sealed with contain the encrypted message.
	message *pond.Message
}

// NewMessage is sent from the network goroutine to the client goroutine and
// contains messages fetched from the home server.
type NewMessage struct {
	fetched *pond.Fetched
	ack     chan bool
}

// Contact represents a contact to which we can send messages.
type Contact struct {
	// id is only locally valid.
	id uint64
	// name is the friendly name that the user chose for this contact.
	name string
	// isPending is true if we haven't received a key exchange message from
	// this contact.
	isPending bool
	// kxsBytes is the serialised key exchange message that we generated
	// for this contact. (Only valid if |isPending| is true.)
	kxsBytes []byte
	// groupKey is the group member key that we gave to this contact.
	// myGroupKey is the one that they gave to us.
	groupKey, myGroupKey *bbssig.MemberKey
	// generation is the current group generation number that we know for
	// this contact.
	generation uint32
	// theirServer is the URL of the contact's home server.
	theirServer string
	// theirPub is their Ed25519 public key.
	theirPub [32]byte
	// theirIdentityPublic is the public identity that their home server
	// knows them by.
	theirIdentityPublic [32]byte

	lastDHPrivate    [32]byte
	currentDHPrivate [32]byte

	theirLastDHPublic    [32]byte
	theirCurrentDHPublic [32]byte
}

type queuedMessage struct {
	request *pond.Request
	id      uint64
	to      uint64
	server  string
	created time.Time
	sent    time.Time
	acked   time.Time
	message *pond.Message
}

func (c *client) loadUI() {
	ui := VBox{
		children: []Widget{
			EventBox{
				widgetBase: widgetBase{background: 0x333355},
				child: HBox{
					children: []Widget{
						Label{
							widgetBase: widgetBase{
								foreground: colorWhite,
								padding:    10,
								font:       fontLoadTitle,
							},
							text: "Pond",
						},
					},
				},
			},
			HBox{
				widgetBase: widgetBase{name: "body", padding: 30, expand: true, fill: true},
			},
		},
	}
	c.ui.Actions() <- Reset{ui}

	loading := EventBox{
		widgetBase: widgetBase{background: colorGray},
		child: Label{
			widgetBase: widgetBase{
				foreground: colorTitleForeground,
				font:       fontLoadLarge,
			},
			text:   "Loading...",
			xAlign: 0.5,
			yAlign: 0.5,
		},
	}

	c.ui.Actions() <- SetBoxContents{name: "body", child: loading}
	c.ui.Actions() <- UIState{uiStateLoading}
	c.ui.Signal()

	state, err := ioutil.ReadFile(c.stateFilename)
	var ok bool
	c.diskSalt, ok = getSCryptSaltFromState(state)

	newAccount := false
	if err != nil || !ok {
		// New account flow.
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		copy(c.priv[:], priv[:])
		copy(c.pub[:], pub[:])

		c.groupPriv, err = bbssig.GenerateGroup(rand.Reader)
		if err != nil {
			panic(err)
		}
		c.createPassphraseUI()
		c.createAccountUI()
		newAccount = true
	} else {
		// First try with zero key.
		err = c.loadState(state, &c.diskKey)
		for err == badPasswordError {
			// That didn't work, try prompting for a key.
			err = c.keyPromptUI(state)
		}
		if err != nil {
			// Fatal error loading state. Abort.
			ui := EventBox{
				widgetBase: widgetBase{background: colorError},
				child: Label{
					widgetBase: widgetBase{
						foreground: colorBlack,
						font:       "Ariel Bold 12",
					},
					text:   err.Error(),
					xAlign: 0.5,
					yAlign: 0.5,
				},
			}
			c.ui.Actions() <- Reset{ui}
			c.ui.Actions() <- UIState{uiStateError}
			c.ui.Signal()
			select {}
		}
	}

	c.writerChan = make(chan []byte)
	c.writerDone = make(chan bool)
	c.fetchNowChan = make(chan chan bool)

	// Start disk and network workers.
	go stateWriter(c.stateFilename, &c.diskKey, &c.diskSalt, c.writerChan, c.writerDone)
	go c.transact()
	if newAccount {
		c.save()
	}

	c.mainUI()
}

func (c *client) DeselectAll() {
	c.inboxUI.Deselect()
	c.outboxUI.Deselect()
	c.contactsUI.Deselect()
	c.clientUI.Deselect()
}

func (c *client) mainUI() {
	ui := Paned{
		left: Scrolled{
			child: EventBox{
				widgetBase: widgetBase{background: colorGray},
				child: VBox{
					children: []Widget{
						EventBox{
							widgetBase: widgetBase{background: colorHeaderBackground},
							child: Label{
								widgetBase: widgetBase{
									foreground: colorHeaderForegroundSmall,
									padding:    10,
									font:       fontListHeading,
								},
								xAlign: 0.5,
								text:   "Inbox",
							},
						},
						EventBox{widgetBase: widgetBase{height: 1, background: colorSep}},
						VBox{widgetBase: widgetBase{name: "inboxVbox"}},
						EventBox{
							widgetBase: widgetBase{background: colorHeaderBackground},
							child: Label{
								widgetBase: widgetBase{
									foreground: colorHeaderForegroundSmall,
									padding:    10,
									font:       fontListHeading,
								},
								xAlign: 0.5,
								text:   "Outbox",
							},
						},
						EventBox{widgetBase: widgetBase{height: 1, background: colorSep}},
						HBox{
							widgetBase: widgetBase{padding: 6},
							children: []Widget{
								HBox{widgetBase: widgetBase{expand: true}},
								HBox{
									widgetBase: widgetBase{padding: 8},
									children: []Widget{
										VBox{
											widgetBase: widgetBase{padding: 8},
											children: []Widget{
												Button{
													widgetBase: widgetBase{width: 100, name: "compose"},
													text:       "Compose",
												},
											},
										},
									},
								},
								HBox{widgetBase: widgetBase{expand: true}},
							},
						},
						VBox{widgetBase: widgetBase{name: "outboxVbox"}},
						EventBox{
							widgetBase: widgetBase{background: colorHeaderBackground},
							child: Label{
								widgetBase: widgetBase{
									foreground: colorHeaderForegroundSmall,
									padding:    10,
									font:       fontListHeading,
								},
								xAlign: 0.5,
								text:   "Contacts",
							},
						},
						HBox{
							widgetBase: widgetBase{padding: 6},
							children: []Widget{
								HBox{widgetBase: widgetBase{expand: true}},
								HBox{
									widgetBase: widgetBase{padding: 8},
									children: []Widget{
										VBox{
											widgetBase: widgetBase{padding: 8},
											children: []Widget{
												Button{
													widgetBase: widgetBase{width: 100, name: "newcontact"},
													text:       "Add",
												},
											},
										},
									},
								},
								HBox{widgetBase: widgetBase{expand: true}},
							},
						},
						VBox{
							widgetBase: widgetBase{name: "contactsVbox"},
						},
						EventBox{
							widgetBase: widgetBase{background: colorHeaderBackground},
							child: Label{
								widgetBase: widgetBase{
									foreground: colorHeaderForegroundSmall,
									padding:    10,
									font:       fontListHeading,
								},
								xAlign: 0.5,
								text:   "Client",
							},
						},
						VBox{
							widgetBase: widgetBase{name: "clientVbox"},
						},
					},
				},
			},
		},
		right: Scrolled{
			horizontal: true,
			child: EventBox{
				widgetBase: widgetBase{background: colorGray, name: "right"},
				child: Label{
					widgetBase: widgetBase{
						foreground: colorTitleForeground,
						font:       fontLoadLarge,
					},
					text:   "Pond",
					xAlign: 0.5,
					yAlign: 0.5,
				},
			},
		},
	}

	c.ui.Actions() <- Reset{ui}
	c.ui.Actions() <- UIState{uiStateMain}
	c.ui.Signal()

	c.contactsUI = &listUI{
		ui:       c.ui,
		vboxName: "contactsVbox",
	}

	for id, contact := range c.contacts {
		subline := ""
		if contact.isPending {
			subline = "pending"
		}
		c.contactsUI.Add(id, contact.name, subline, indicatorNone)
	}

	c.inboxUI = &listUI{
		ui:       c.ui,
		vboxName: "inboxVbox",
	}

	for _, msg := range c.inbox {
		var subline string
		i := indicatorNone

		if msg.message == nil {
			subline = "pending"
		} else {
			if len(msg.message.Body) == 0 {
				continue
			}
			if !msg.read {
				i = indicatorBlue
			}
			subline = time.Unix(*msg.message.Time, 0).Format(shortTimeFormat)
		}
		c.inboxUI.Add(msg.id, c.contacts[msg.from].name, subline, i)
	}

	c.outboxUI = &listUI{
		ui:       c.ui,
		vboxName: "outboxVbox",
	}

	for _, msg := range c.outbox {
		if len(msg.message.Body) > 0 {
			subline := msg.created.Format(shortTimeFormat)
			c.outboxUI.Add(msg.id, c.contacts[msg.to].name, subline, msg.indicator())
		}
	}

	c.clientUI = &listUI{
		ui:       c.ui,
		vboxName: "clientVbox",
	}
	const (
		clientUIIdentity = iota + 1
		clientUIActivity
	)
	c.clientUI.Add(clientUIIdentity, "Identity", "", indicatorNone)
	c.clientUI.Add(clientUIActivity, "Activity Log", "", indicatorNone)

	var nextEvent interface{}
	for {
		event := nextEvent
		nextEvent = nil
		if event == nil {
			event, _ = c.nextEvent()
		}
		if event == nil {
			continue
		}

		c.DeselectAll()
		if id, ok := c.inboxUI.Event(event); ok {
			c.inboxUI.Select(id)
			nextEvent = c.showInbox(id)
			continue
		}
		if id, ok := c.outboxUI.Event(event); ok {
			c.outboxUI.Select(id)
			nextEvent = c.showOutbox(id)
			continue
		}
		if id, ok := c.contactsUI.Event(event); ok {
			c.contactsUI.Select(id)
			nextEvent = c.showContact(id)
			continue
		}
		if id, ok := c.clientUI.Event(event); ok {
			c.clientUI.Select(id)
			switch id {
			case clientUIIdentity:
				nextEvent = c.identityUI()
			case clientUIActivity:
				nextEvent = c.logUI()
			default:
				panic("bad clientUI event")
			}
			continue
		}

		click, ok := event.(Click)
		if !ok {
			continue
		}
		switch click.name {
		case "newcontact":
			nextEvent = c.newContactUI(nil)
		case "compose":
			nextEvent = c.composeUI(nil)
		}
	}
}

// listUI manages the sections in the left-hand side list. It contains a number
// of items, which may have subheadlines and indicators (coloured dots).
type listUI struct {
	ui       UI
	vboxName string
	entries  []listItem
	selected uint64
	nextId   int
}

type listItem struct {
	id                                                 uint64
	name, sepName, boxName, imageName, sublineTextName string
}

func (cs *listUI) Event(event interface{}) (uint64, bool) {
	if click, ok := event.(Click); ok {
		for _, entry := range cs.entries {
			if click.name == entry.boxName {
				return entry.id, true
			}
		}
	}

	return 0, false
}

func (cs *listUI) Add(id uint64, name, subline string, indicator Indicator) {
	c := listItem{
		id:              id,
		name:            name,
		sepName:         cs.newIdent(),
		boxName:         cs.newIdent(),
		imageName:       cs.newIdent(),
		sublineTextName: cs.newIdent(),
	}
	cs.entries = append(cs.entries, c)
	index := len(cs.entries) - 1

	// Add the separator bar.
	cs.ui.Actions() <- AddToBox{
		box:   cs.vboxName,
		pos:   index * 2,
		child: EventBox{widgetBase: widgetBase{height: 1, background: 0xe5e6e6, name: c.sepName}},
	}

	children := []Widget{
		HBox{
			widgetBase: widgetBase{padding: 1},
			children: []Widget{
				Label{
					widgetBase: widgetBase{
						padding: 5,
						font:    fontListEntry,
					},
					text: name,
				},
			},
		},
	}

	var sublineChildren []Widget

	if len(subline) > 0 {
		sublineChildren = append(sublineChildren, Label{
			widgetBase: widgetBase{
				padding:    5,
				foreground: colorSubline,
				font:       fontListSubline,
				name:       c.sublineTextName,
			},
			text: subline,
		})
	}

	sublineChildren = append(sublineChildren, Image{
		widgetBase: widgetBase{
			padding: 4,
			expand:  true,
			fill:    true,
			name:    c.imageName,
		},
		image:  indicator,
		xAlign: 1,
		yAlign: 0.5,
	})

	children = append(children, HBox{
		widgetBase: widgetBase{padding: 1},
		children:   sublineChildren,
	})

	cs.ui.Actions() <- AddToBox{
		box: cs.vboxName,
		pos: index*2 + 1,
		child: EventBox{
			widgetBase: widgetBase{name: c.boxName, background: colorGray},
			child:      VBox{children: children},
		},
	}
	cs.ui.Signal()
}

func (cs *listUI) Remove(id uint64) {
	for _, entry := range cs.entries {
		if entry.id == id {
			cs.ui.Actions() <- Destroy{name: entry.sepName}
			cs.ui.Actions() <- Destroy{name: entry.boxName}
			cs.ui.Signal()
			return
		}
	}

	panic("unknown id passed to Remove")
}

func (cs *listUI) Deselect() {
	if cs.selected == 0 {
		return
	}

	var currentlySelected string

	for _, entry := range cs.entries {
		if entry.id == cs.selected {
			currentlySelected = entry.boxName
			break
		}
	}

	cs.ui.Actions() <- SetBackground{name: currentlySelected, color: colorGray}
	cs.selected = 0
	cs.ui.Signal()
}

func (cs *listUI) Select(id uint64) {
	if id == cs.selected {
		return
	}

	var currentlySelected, newSelected string

	for _, entry := range cs.entries {
		if entry.id == cs.selected {
			currentlySelected = entry.boxName
		} else if entry.id == id {
			newSelected = entry.boxName
		}

		if len(currentlySelected) > 0 && len(newSelected) > 0 {
			break
		}
	}

	if len(newSelected) == 0 {
		panic("internal error")
	}

	if len(currentlySelected) > 0 {
		cs.ui.Actions() <- SetBackground{name: currentlySelected, color: colorGray}
	}
	cs.ui.Actions() <- SetBackground{name: newSelected, color: colorHighlight}
	cs.selected = id
	cs.ui.Signal()
}

func (cs *listUI) SetIndicator(id uint64, indicator Indicator) {
	for _, entry := range cs.entries {
		if entry.id == id {
			cs.ui.Actions() <- SetImage{name: entry.imageName, image: indicator}
			cs.ui.Signal()
			break
		}
	}
}

func (cs *listUI) SetSubline(id uint64, subline string) {
	for _, entry := range cs.entries {
		if entry.id == id {
			if len(subline) > 0 {
				cs.ui.Actions() <- SetText{name: entry.sublineTextName, text: subline}
			} else {
				cs.ui.Actions() <- Destroy{name: entry.sublineTextName}
			}
			cs.ui.Signal()
			break
		}
	}
}

func (cs *listUI) newIdent() string {
	id := cs.vboxName + "-" + strconv.Itoa(cs.nextId)
	cs.nextId++
	return id
}

type nvEntry struct {
	name, value string
}

func (c *client) showContact(id uint64) interface{} {
	contact := c.contacts[id]
	if contact.isPending {
		return c.newContactUI(contact)
	}

	entries := []nvEntry{
		{"NAME", contact.name},
		{"SERVER", contact.theirServer},
		{"PUBLIC IDENTITY", fmt.Sprintf("%x", contact.theirIdentityPublic[:])},
		{"PUBLIC KEY", fmt.Sprintf("%x", contact.theirPub[:])},
		{"LAST DH", fmt.Sprintf("%x", contact.theirLastDHPublic[:])},
		{"CURRENT DH", fmt.Sprintf("%x", contact.theirCurrentDHPublic[:])},
		{"GROUP GENERATION", fmt.Sprintf("%d", contact.generation)},
	}

	if len(contact.kxsBytes) > 0 {
		var out bytes.Buffer
		pem.Encode(&out, &pem.Block{Bytes: contact.kxsBytes, Type: keyExchangePEM})
		entries = append(entries, nvEntry{"KEY EXCHANGE", string(out.Bytes())})
	}

	c.showNameValues("CONTACT", entries)
	c.ui.Actions() <- UIState{uiStateShowContact}
	c.ui.Signal()

	return nil
}

func (c *client) identityUI() interface{} {
	entries := []nvEntry{
		{"SERVER", c.server},
		{"PUBLIC IDENTITY", fmt.Sprintf("%x", c.identityPublic[:])},
		{"PUBLIC KEY", fmt.Sprintf("%x", c.pub[:])},
		{"STATE FILE", c.stateFilename},
		{"GROUP GENERATION", fmt.Sprintf("%d", c.generation)},
	}

	c.showNameValues("IDENTITY", entries)
	c.ui.Actions() <- UIState{uiStateShowIdentity}
	c.ui.Signal()

	return nil
}

func (c *client) showNameValues(title string, entries []nvEntry) {
	ui := VBox{
		children: []Widget{
			EventBox{
				widgetBase: widgetBase{background: colorHeaderBackground},
				child: VBox{
					children: []Widget{
						HBox{
							widgetBase: widgetBase{padding: 10},
							children: []Widget{
								Label{
									widgetBase: widgetBase{font: fontMainTitle, padding: 10, foreground: colorHeaderForeground},
									text:       title,
								},
							},
						},
					},
				},
			},
			EventBox{widgetBase: widgetBase{height: 1, background: colorSep}},
			HBox{
				widgetBase: widgetBase{padding: 2},
			},
		},
	}

	for _, ent := range entries {
		var font string
		yAlign := float32(0.5)
		if strings.HasPrefix(ent.value, "-----") {
			// PEM block
			font = fontMainMono
			yAlign = 0
		}

		ui.children = append(ui.children, HBox{
			widgetBase: widgetBase{padding: 3},
			children: []Widget{
				Label{
					widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
					text:       ent.name,
					yAlign:     yAlign,
				},
				Label{
					widgetBase: widgetBase{font: font},
					text:       ent.value,
					selectable: true,
				},
			},
		})
	}

	c.ui.Actions() <- SetChild{name: "right", child: ui}
}

// usageString returns a description of the amount of space taken up by a body
// with the given contents and a bool indicating overflow.
func usageString(body string, isReply bool, attachments map[uint64]*pond.Message_Attachment) (string, bool) {
	var replyToId *uint64
	if isReply {
		replyToId = proto.Uint64(1)
	}
	var dhPub [32]byte

	msg := &pond.Message{
		Id:           proto.Uint64(0),
		Time:         proto.Int64(1 << 62),
		Body:         []byte(body),
		BodyEncoding: pond.Message_RAW.Enum(),
		InReplyTo:    replyToId,
		MyNextDh:     dhPub[:],
		Files:        attachmentsMapToList(attachments),
	}

	serialized, err := proto.Marshal(msg)
	if err != nil {
		panic("error while serialising candidate Message: " + err.Error())
	}

	s := fmt.Sprintf("%d of %d bytes", len(serialized), pond.MaxSerializedMessage)
	return s, len(serialized) > pond.MaxSerializedMessage
}

func attachmentsMapToList(attachments map[uint64]*pond.Message_Attachment) []*pond.Message_Attachment {
	if attachments == nil {
		return nil
	}

	var ret []*pond.Message_Attachment
	for _, attachment := range attachments {
		ret = append(ret, attachment)
	}
	return ret
}

func (c *client) updateUsage(text string, isReply bool, attachments map[uint64]*pond.Message_Attachment) {
	usageMessage, over := usageString(text, isReply, attachments)
	c.ui.Actions() <- SetText{name: "usage", text: usageMessage}
	color := uint32(colorBlack)
	if over {
		color = colorRed
		c.ui.Actions() <- Sensitive{name: "send", sensitive: false}
	} else {
		c.ui.Actions() <- Sensitive{name: "send", sensitive: true}
	}
	c.ui.Actions() <- SetForeground{name: "usage", foreground: color}
}

func (c *client) composeUI(inReplyTo *InboxMessage) interface{} {
	var contactNames []string
	for _, contact := range c.contacts {
		contactNames = append(contactNames, contact.name)
	}

	var preSelected string
	if inReplyTo != nil {
		if from, ok := c.contacts[inReplyTo.from]; ok {
			preSelected = from.name
		}
	}

	initialUsageMessage, _ := usageString("", inReplyTo != nil, nil)
	var lastText string

	ui := VBox{
		children: []Widget{
			EventBox{
				widgetBase: widgetBase{background: colorHeaderBackground},
				child: VBox{
					children: []Widget{
						HBox{
							widgetBase: widgetBase{padding: 10},
							children: []Widget{
								Label{
									widgetBase: widgetBase{font: fontMainTitle, padding: 10, foreground: colorHeaderForeground},
									text:       "COMPOSE",
								},
							},
						},
					},
				},
			},
			EventBox{widgetBase: widgetBase{height: 1, background: colorSep}},
			HBox{
				widgetBase: widgetBase{padding: 2},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "TO",
						yAlign:     0.5,
					},
					Combo{
						widgetBase: widgetBase{
							name:        "to",
							insensitive: len(preSelected) > 0,
						},
						labels:      contactNames,
						preSelected: preSelected,
					},
					Label{
						widgetBase: widgetBase{expand: true},
					},
					Button{
						widgetBase: widgetBase{packEnd: true, padding: 10, name: "send"},
						text:       "Send",
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 2},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "SIZE",
						yAlign:     0.5,
					},
					Label{
						widgetBase: widgetBase{name: "usage"},
						text:       initialUsageMessage,
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 0},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "ATTACHMENTS",
						yAlign:     0.5,
					},
					Button{
						widgetBase: widgetBase{name: "attach", font: "Liberation Sans 8"},
						text:       "+",
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 0},
				children: []Widget{
					VBox{
						widgetBase: widgetBase{name: "filesvbox", padding: 25},
					},
				},
			},
			TextView{
				widgetBase:     widgetBase{expand: true, fill: true, name: "body"},
				editable:       true,
				wrap:           true,
				updateOnChange: true,
			},
		},
	}
	c.ui.Actions() <- SetChild{name: "right", child: ui}
	c.ui.Actions() <- UIState{uiStateCompose}
	c.ui.Signal()

	attachments := make(map[uint64]*pond.Message_Attachment)

	for {
		event, wanted := c.nextEvent()
		if wanted {
			return event
		}

		if update, ok := event.(Update); ok {
			lastText = update.text
			c.updateUsage(lastText, inReplyTo != nil, attachments)
			c.ui.Signal()
			continue
		}

		if open, ok := event.(OpenResult); ok && open.ok {
			base := filepath.Base(open.path)
			contents, err := ioutil.ReadFile(open.path)
			id := c.randId()

			var label Widget
			if err != nil {
				label = Label{
					widgetBase: widgetBase{foreground: colorRed, padding: 2},
					yAlign:     0.5,
					text:       base + ": " + err.Error(),
				}
			} else {
				label = Label{
					widgetBase: widgetBase{padding: 2},
					yAlign:     0.5,
					text:       fmt.Sprintf("%s (%d bytes)", base, len(contents)),
				}
				attachments[id] = &pond.Message_Attachment{
					Filename: proto.String(filepath.Base(open.path)),
					Contents: contents,
				}
			}

			c.ui.Actions() <- Append{
				name: "filesvbox",
				children: []Widget{
					HBox{
						widgetBase: widgetBase{
							name: fmt.Sprintf("attachment-hbox-%x", id),
						},
						children: []Widget{
							label,
							Button{
								widgetBase: widgetBase{name: fmt.Sprintf("remove-%x", id)},
								text:       "Remove",
							},
						},
					},
				},
			}
			c.updateUsage(lastText, inReplyTo != nil, attachments)
			c.ui.Signal()
		}

		click, ok := event.(Click)
		if !ok {
			continue
		}
		if click.name == "attach" {
			c.ui.Actions() <- FileOpen{
				title: "Attach File",
			}
			c.ui.Signal()
		}
		if strings.HasPrefix(click.name, "remove-") {
			// One of the attachment remove buttons.
			id, err := strconv.ParseUint(click.name[7:], 16, 64)
			if err != nil {
				panic(click.name)
			}
			c.ui.Actions() <- Destroy{name: "attachment-hbox-" + click.name[7:]}
			delete(attachments, id)
			c.updateUsage(lastText, inReplyTo != nil, attachments)
			c.ui.Signal()
		}
		if click.name != "send" {
			continue
		}

		toName := click.combos["to"]
		if len(toName) == 0 {
			continue
		}

		var to *Contact
		for _, contact := range c.contacts {
			if contact.name == toName {
				to = contact
				break
			}
		}

		var nextDHPub [32]byte
		curve25519.ScalarBaseMult(&nextDHPub, &to.currentDHPrivate)

		var replyToId *uint64
		if inReplyTo != nil {
			replyToId = inReplyTo.message.Id
		}

		body := click.textViews["body"]
		// Zero length bodies are ACKs.
		if len(body) == 0 {
			body = " "
		}

		id := c.randId()
		err := c.send(to, &pond.Message{
			Id:           proto.Uint64(id),
			Time:         proto.Int64(time.Now().Unix()),
			Body:         []byte(body),
			BodyEncoding: pond.Message_RAW.Enum(),
			InReplyTo:    replyToId,
			MyNextDh:     nextDHPub[:],
			Files:        attachmentsMapToList(attachments),
		})
		if err != nil {
			// TODO: handle this case better.
			println(err.Error())
			c.log.Errorf("Error sending message: %s", err)
			continue
		}
		if inReplyTo != nil {
			inReplyTo.acked = true
		}

		c.save()

		c.outboxUI.Select(id)
		return c.showOutbox(id)
	}

	return nil
}

func (qm *queuedMessage) indicator() Indicator {
	switch {
	case !qm.acked.IsZero():
		return indicatorGreen
	case !qm.sent.IsZero():
		return indicatorYellow
	}
	return indicatorRed
}

func (c *client) enqueue(m *queuedMessage) {
	c.queueMutex.Lock()
	defer c.queueMutex.Unlock()

	c.queue = append(c.queue, m)
}

func (c *client) sendAck(msg *InboxMessage) {
	to := c.contacts[msg.from]

	var nextDHPub [32]byte
	curve25519.ScalarBaseMult(&nextDHPub, &to.currentDHPrivate)

	id := c.randId()
	err := c.send(to, &pond.Message{
		Id:           proto.Uint64(id),
		Time:         proto.Int64(time.Now().Unix()),
		Body:         make([]byte, 0),
		BodyEncoding: pond.Message_RAW.Enum(),
		MyNextDh:     nextDHPub[:],
		InReplyTo:    msg.message.Id,
	})
	if err != nil {
		c.log.Errorf("Error sending message: %s", err)
	}
}

func (c *client) showInbox(id uint64) interface{} {
	var msg *InboxMessage
	for _, candidate := range c.inbox {
		if candidate.id == id {
			msg = candidate
			break
		}
	}
	if msg == nil {
		panic("failed to find message in inbox")
	}
	if msg.message != nil && !msg.read {
		msg.read = true
		c.inboxUI.SetIndicator(id, indicatorNone)
		c.save()
	}

	contact := c.contacts[msg.from]
	isPending := msg.message == nil
	var msgText, sentTimeText string
	if isPending {
		msgText = "(cannot display message as key exchange is still pending)"
		sentTimeText = "(unknown)"
	} else {
		sentTimeText = time.Unix(*msg.message.Time, 0).Format(time.RFC1123)
		msgText = "(cannot display message as encoding is not supported)"
		if msg.message.BodyEncoding != nil {
			switch *msg.message.BodyEncoding {
			case pond.Message_RAW:
				msgText = string(msg.message.Body)
			}
		}
	}
	eraseTimeText := msg.receivedTime.Add(messageLifetime).Format(time.RFC1123)

	ui := VBox{
		children: []Widget{
			EventBox{
				widgetBase: widgetBase{background: colorHeaderBackground},
				child: VBox{
					children: []Widget{
						HBox{
							widgetBase: widgetBase{padding: 10},
							children: []Widget{
								Label{
									widgetBase: widgetBase{font: fontMainTitle, padding: 10, foreground: colorHeaderForeground},
									text:       "RECEIVED MESSAGE",
								},
							},
						},
					},
				},
			},
			EventBox{widgetBase: widgetBase{height: 1, background: colorSep}},
			HBox{
				widgetBase: widgetBase{padding: 2},
			},
			HBox{
				children: []Widget{
					VBox{
						widgetBase: widgetBase{name: "lhs"},
						children: []Widget{
							HBox{
								widgetBase: widgetBase{padding: 3},
								children: []Widget{
									Label{
										widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
										text:       "FROM",
										yAlign:     0.5,
									},
									Label{
										text: contact.name,
									},
								},
							},
							HBox{
								widgetBase: widgetBase{padding: 3},
								children: []Widget{
									Label{
										widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
										text:       "SENT",
										yAlign:     0.5,
									},
									Label{
										text: sentTimeText,
									},
								},
							},
							HBox{
								widgetBase: widgetBase{padding: 3},
								children: []Widget{
									Label{
										widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
										text:       "ERASE",
										yAlign:     0.5,
									},
									Label{
										text: eraseTimeText,
									},
								},
							},
						},
					},
					VBox{
						widgetBase: widgetBase{
							expand: true,
							fill:   true,
						},
					},
					VBox{
						widgetBase: widgetBase{
							padding: 10,
						},
						children: []Widget{
							Button{
								widgetBase: widgetBase{
									name:        "reply",
									padding:     2,
									insensitive: isPending,
								},
								text: "Reply",
							},
							Button{
								widgetBase: widgetBase{
									name:        "ack",
									padding:     2,
									insensitive: isPending || msg.acked,
								},
								text: "Ack",
							},
							Button{
								widgetBase: widgetBase{
									name:        "delete",
									padding:     2,
									insensitive: true,
								},
								text: "Delete Now",
							},
						},
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 2},
			},
			TextView{
				widgetBase: widgetBase{expand: true, fill: true, name: "body"},
				editable:   false,
				text:       msgText,
				wrap:       true,
			},
		},
	}
	c.ui.Actions() <- SetChild{name: "right", child: ui}

	if msg.message != nil && len(msg.message.Files) != 0 {
		var attachmentWidgets []Widget
		for i, attachment := range msg.message.Files {
			filename := *attachment.Filename
			if runes := []rune(filename); len(runes) > 30 {
				runes = runes[:30]
				runes = append(runes, 0x2026 /* ellipsis */)
				filename = string(runes)
			}
			attachmentWidgets = append(attachmentWidgets, HBox{
				children: []Widget{
					Label{text: filename, yAlign: 0.5},
					Button{
						widgetBase: widgetBase{name: fmt.Sprintf("attachment-%d", i), padding: 3},
						text:       "Save",
					},
				},
			})
		}
		attachmentsUI := []Widget{
			HBox{
				widgetBase: widgetBase{padding: 3},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "ATTACHMENTS",
						yAlign:     0.5,
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 3},
				children: []Widget{
					VBox{
						widgetBase: widgetBase{padding: 25},
						children:   attachmentWidgets,
					},
				},
			},
		}
		c.ui.Actions() <- Append{name: "lhs", children: attachmentsUI}
	}

	c.ui.Actions() <- UIState{uiStateInbox}
	c.ui.Signal()

	for {
		event, wanted := c.nextEvent()
		if wanted {
			return event
		}

		if open, ok := event.(OpenResult); ok && open.ok {
			ioutil.WriteFile(open.path, msg.message.Files[open.arg.(int)].Contents, 0600)
		}

		click, ok := event.(Click)
		if !ok {
			continue
		}
		if strings.HasPrefix(click.name, "attachment-") {
			i, _ := strconv.Atoi(click.name[11:])
			c.ui.Actions() <- FileOpen{
				save:  true,
				title: "Save Attachment",
				arg:   i}
			c.ui.Signal()
			continue
		}
		switch click.name {
		case "ack":
			c.ui.Actions() <- Sensitive{name: "ack", sensitive: false}
			c.ui.Signal()
			msg.acked = true
			c.sendAck(msg)
			c.ui.Actions() <- UIState{uiStateInbox}
			c.ui.Signal()
		case "reply":
			return c.composeUI(msg)
		}
	}

	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "(not yet)"
	}
	return t.Format(time.RFC1123)
}

func (c *client) showOutbox(id uint64) interface{} {
	var msg *queuedMessage
	for _, candidate := range c.outbox {
		if candidate.id == id {
			msg = candidate
			break
		}
	}
	if msg == nil {
		panic("failed to find message in outbox")
	}

	contact := c.contacts[msg.to]

	ui := VBox{
		children: []Widget{
			EventBox{
				widgetBase: widgetBase{background: colorHeaderBackground},
				child: VBox{
					children: []Widget{
						HBox{
							widgetBase: widgetBase{padding: 10},
							children: []Widget{
								Label{
									widgetBase: widgetBase{font: fontMainTitle, padding: 10, foreground: colorHeaderForeground},
									text:       "SENT MESSAGE",
								},
							},
						},
					},
				},
			},
			EventBox{widgetBase: widgetBase{height: 1, background: colorSep}},
			HBox{
				widgetBase: widgetBase{padding: 2},
			},
			HBox{
				widgetBase: widgetBase{padding: 3},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "TO",
						yAlign:     0.5,
					},
					Label{
						text: contact.name,
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 3},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "CREATED",
						yAlign:     0.5,
					},
					Label{
						text: time.Unix(*msg.message.Time, 0).Format(time.RFC1123),
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 3},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "SENT",
						yAlign:     0.5,
					},
					Label{
						widgetBase: widgetBase{name: "sent"},
						text:       formatTime(msg.sent),
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 3},
				children: []Widget{
					Label{
						widgetBase: widgetBase{font: fontMainLabel, foreground: colorHeaderForeground, padding: 10},
						text:       "ACKNOWLEDGED",
						yAlign:     0.5,
					},
					Label{
						widgetBase: widgetBase{name: "acked"},
						text:       formatTime(msg.acked),
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 2},
			},
			TextView{
				widgetBase: widgetBase{expand: true, fill: true, name: "body"},
				editable:   false,
				text:       string(msg.message.Body),
				wrap:       true,
			},
		},
	}
	c.ui.Actions() <- SetChild{name: "right", child: ui}
	c.ui.Actions() <- UIState{uiStateOutbox}
	c.ui.Signal()

	haveSentTime := !msg.sent.IsZero()
	haveAckTime := !msg.acked.IsZero()

	for {
		event, wanted := c.nextEvent()
		if wanted {
			return event
		}

		if !haveSentTime && !msg.sent.IsZero() {
			c.ui.Actions() <- SetText{name: "sent", text: formatTime(msg.sent)}
			c.ui.Signal()
		}
		if !haveAckTime && !msg.acked.IsZero() {
			c.ui.Actions() <- SetText{name: "acked", text: formatTime(msg.acked)}
			c.ui.Signal()
		}
	}

	return nil
}

func (contact *Contact) processKeyExchange(kxsBytes []byte, testing bool) error {
	var kxs pond.SignedKeyExchange
	if err := proto.Unmarshal(kxsBytes, &kxs); err != nil {
		return err
	}

	var sig [64]byte
	if len(kxs.Signature) != len(sig) {
		return errors.New("invalid signature length")
	}
	copy(sig[:], kxs.Signature)

	var kx pond.KeyExchange
	if err := proto.Unmarshal(kxs.Signed, &kx); err != nil {
		return err
	}

	if len(kx.PublicKey) != len(contact.theirPub) {
		return errors.New("invalid public key")
	}
	copy(contact.theirPub[:], kx.PublicKey)

	if !ed25519.Verify(&contact.theirPub, kxs.Signed, &sig) {
		return errors.New("invalid signature")
	}

	contact.theirServer = *kx.Server
	if _, _, err := parseServer(contact.theirServer, testing); err != nil {
		return err
	}

	group, ok := new(bbssig.Group).Unmarshal(kx.Group)
	if !ok {
		return errors.New("invalid group")
	}
	if contact.myGroupKey, ok = new(bbssig.MemberKey).Unmarshal(group, kx.GroupKey); !ok {
		return errors.New("invalid group key")
	}

	if len(kx.IdentityPublic) != len(contact.theirIdentityPublic) {
		return errors.New("invalid public identity")
	}
	copy(contact.theirIdentityPublic[:], kx.IdentityPublic)

	if len(kx.Dh) != len(contact.theirCurrentDHPublic) {
		return errors.New("invalid public DH value")
	}
	copy(contact.theirCurrentDHPublic[:], kx.Dh)

	contact.generation = *kx.Generation

	return nil
}

func (c *client) newContactUI(contact *Contact) interface{} {
	var name, handshake string
	var out bytes.Buffer

	existing := contact != nil
	if existing {
		name = contact.name
		pem.Encode(&out, &pem.Block{Bytes: contact.kxsBytes, Type: keyExchangePEM})
		handshake = string(out.Bytes())
	}

	ui := VBox{
		widgetBase: widgetBase{padding: 8, expand: true, fill: true},
		children: []Widget{
			EventBox{
				widgetBase: widgetBase{background: colorHeaderBackground},
				child: VBox{
					children: []Widget{
						HBox{
							widgetBase: widgetBase{padding: 10},
							children: []Widget{
								Label{
									widgetBase: widgetBase{font: fontMainTitle, padding: 10, foreground: colorHeaderForeground},
									text:       "CREATE CONTACT",
								},
							},
						},
					},
				},
			},
			EventBox{widgetBase: widgetBase{height: 1, background: colorSep}},
			HBox{
				widgetBase: widgetBase{padding: 2},
			},
			HBox{
				children: []Widget{
					VBox{
						widgetBase: widgetBase{padding: 8},
						children: []Widget{
							Label{
								widgetBase: widgetBase{
									padding: 16,
									font:    fontMainTitle,
								},
								text: "1. Set a name",
							},
							HBox{
								children: []Widget{
									Label{
										widgetBase: widgetBase{font: fontMainBody},
										text:       "Your name for this contact: ",
										yAlign:     0.5,
									},
									Entry{
										widgetBase: widgetBase{name: "name", insensitive: existing},
										width:      20,
										text:       name,
									},
								},
							},
							HBox{
								widgetBase: widgetBase{padding: 8},
								children: []Widget{
									Button{
										widgetBase: widgetBase{name: "create", insensitive: existing},
										text:       "Create",
									},
								},
							},
							Label{
								widgetBase: widgetBase{
									padding:    16,
									foreground: colorRed,
									name:       "error1",
								},
							},
							Label{
								widgetBase: widgetBase{
									padding: 16,
									font:    fontMainTitle,
								},
								text: "2. Give them a handshake message",
							},
							Label{
								widgetBase: widgetBase{
									padding: 4,
									font:    fontMainBody,
								},
								text: "A handshake is for a single person. Don't give it to anyone else and ensure that it came from the person you intended! For example, you could send it in a PGP signed and encrypted email, or exchange it over an OTR chat.",
								wrap: 400,
							},
							TextView{
								widgetBase: widgetBase{
									height:      150,
									insensitive: !existing,
									name:        "kxout",
									font:        fontMainMono,
								},
								editable: false,
								text:     handshake,
							},
							Label{
								widgetBase: widgetBase{
									padding: 16,
									font:    fontMainTitle,
								},
								text: "3. Enter the handshake message from them",
							},
							Label{
								widgetBase: widgetBase{
									padding: 4,
									font:    fontMainBody,
								},
								text: "You won't be able to exchange messages with them until they complete the handshake.",
								wrap: 400,
							},
							TextView{
								widgetBase: widgetBase{
									height:      150,
									insensitive: !existing,
									name:        "kxin",
									font:        fontMainMono,
								},
								editable: true,
							},
							HBox{
								widgetBase: widgetBase{padding: 8},
								children: []Widget{
									Button{
										widgetBase: widgetBase{name: "process", insensitive: !existing},
										text:       "Process",
									},
								},
							},
							Label{
								widgetBase: widgetBase{
									padding:    16,
									foreground: colorRed,
									name:       "error2",
								},
							},
						},
					},
				},
			},
		},
	}

	c.ui.Actions() <- SetChild{name: "right", child: ui}
	c.ui.Actions() <- SetFocus{name: "name"}
	c.ui.Actions() <- UIState{uiStateNewContact}
	c.ui.Signal()

	if !existing {
		for {
			event, wanted := c.nextEvent()
			if wanted {
				return event
			}

			click, ok := event.(Click)
			if !ok {
				continue
			}
			if click.name != "create" && click.name != "name" {
				continue
			}

			name = click.entries["name"]

			nameIsUnique := true
			for _, contact := range c.contacts {
				if contact.name == name {
					const errText = "A contact by that name already exists!"
					c.ui.Actions() <- SetText{name: "error1", text: errText}
					c.ui.Actions() <- UIError{errors.New(errText)}
					c.ui.Signal()
					nameIsUnique = false
					break
				}
			}

			if nameIsUnique {
				break
			}
		}

		contact = &Contact{
			name:      name,
			isPending: true,
			id:        c.randId(),
		}
		c.contacts[contact.id] = contact

		c.contactsUI.Add(contact.id, name, "pending", indicatorNone)
		c.contactsUI.Select(contact.id)

		kx := c.newKeyExchange(contact)

		pem.Encode(&out, &pem.Block{Bytes: kx, Type: keyExchangePEM})
		handshake = string(out.Bytes())

		c.save()
		c.ui.Actions() <- SetText{name: "error1", text: ""}
		c.ui.Actions() <- SetTextView{name: "kxout", text: handshake}
		c.ui.Actions() <- Sensitive{name: "name", sensitive: false}
		c.ui.Actions() <- Sensitive{name: "create", sensitive: false}
		c.ui.Actions() <- Sensitive{name: "kxout", sensitive: true}
		c.ui.Actions() <- Sensitive{name: "kxin", sensitive: true}
		c.ui.Actions() <- Sensitive{name: "process", sensitive: true}
		c.ui.Actions() <- UIState{uiStateNewContact2}
		c.ui.Signal()
		c.save()
	}

	for {
		event, wanted := c.nextEvent()
		if wanted {
			return event
		}

		click, ok := event.(Click)
		if !ok {
			continue
		}
		if click.name != "process" {
			continue
		}

		block, _ := pem.Decode([]byte(click.textViews["kxin"]))
		if block == nil || block.Type != keyExchangePEM {
			const errText = "No key exchange message found!"
			c.ui.Actions() <- SetText{name: "error2", text: errText}
			c.ui.Actions() <- UIError{errors.New(errText)}
			c.ui.Signal()
			continue
		}
		if err := contact.processKeyExchange(block.Bytes, c.testing); err != nil {
			c.ui.Actions() <- SetText{name: "error2", text: err.Error()}
			c.ui.Actions() <- UIError{err}
			c.ui.Signal()
			continue
		} else {
			break
		}
	}

	contact.isPending = false

	// Unseal all pending messages from this new contact.
	for _, msg := range c.inbox {
		if msg.message == nil && msg.from == contact.id {
			if !c.unsealMessage(msg, contact) || len(msg.message.Body) == 0 {
				c.inboxUI.Remove(msg.id)
				continue
			}
			subline := time.Unix(*msg.message.Time, 0).Format(shortTimeFormat)
			c.inboxUI.SetSubline(msg.id, subline)
			c.inboxUI.SetIndicator(msg.id, indicatorBlue)
		}
	}

	c.contactsUI.SetSubline(contact.id, "")
	c.save()
	return c.showContact(contact.id)
}

func (c *client) nextEvent() (event interface{}, wanted bool) {
	var ok bool
	select {
	case event, ok = <-c.ui.Events():
		if !ok {
			c.ShutdownAndSuspend()
		}
	case newMessage := <-c.newMessageChan:
		c.processFetch(newMessage)
		return
	case id := <-c.messageSentChan:
		c.processMessageSent(id)
		return
	case <-c.log.updateChan:
		return
	}

	if _, ok := c.contactsUI.Event(event); ok {
		wanted = true
	}
	if _, ok := c.outboxUI.Event(event); ok {
		wanted = true
	}
	if _, ok := c.inboxUI.Event(event); ok {
		wanted = true
	}
	if _, ok := c.clientUI.Event(event); ok {
		wanted = true
	}
	if click, ok := event.(Click); ok {
		wanted = wanted || click.name == "newcontact" || click.name == "compose"
	}
	return
}

func (c *client) randBytes(buf []byte) {
	if _, err := io.ReadFull(c.rand, buf); err != nil {
		panic(err)
	}
}

func (c *client) randId() uint64 {
	var idBytes [8]byte
	for {
		c.randBytes(idBytes[:])
		n := binary.LittleEndian.Uint64(idBytes[:])
		if n != 0 {
			return n
		}
	}
	panic("unreachable")
}

func (c *client) newKeyExchange(contact *Contact) []byte {
	var err error
	c.randBytes(contact.lastDHPrivate[:])

	var pub [32]byte
	curve25519.ScalarBaseMult(&pub, &contact.lastDHPrivate)

	if contact.groupKey, err = c.groupPriv.NewMember(c.rand); err != nil {
		panic(err)
	}

	kx := &pond.KeyExchange{
		PublicKey:      c.pub[:],
		IdentityPublic: c.identityPublic[:],
		Server:         proto.String(c.server),
		Dh:             pub[:],
		Group:          contact.groupKey.Group.Marshal(),
		GroupKey:       contact.groupKey.Marshal(),
		Generation:     proto.Uint32(c.generation),
	}

	kxBytes, err := proto.Marshal(kx)
	if err != nil {
		panic(err)
	}

	sig := ed25519.Sign(&c.priv, kxBytes)

	kxs := &pond.SignedKeyExchange{
		Signed:    kxBytes,
		Signature: sig[:],
	}

	if contact.kxsBytes, err = proto.Marshal(kxs); err != nil {
		panic(err)
	}
	return contact.kxsBytes
}

func (c *client) keyPromptUI(state []byte) error {
	ui := VBox{
		widgetBase: widgetBase{padding: 40, expand: true, fill: true, name: "vbox"},
		children: []Widget{
			Label{
				widgetBase: widgetBase{font: "DejaVu Sans 30"},
				text:       "Passphrase",
			},
			Label{
				widgetBase: widgetBase{
					padding: 20,
					font:    "DejaVu Sans 14",
				},
				text: "Please enter the passphrase used to encrypt Pond's state file. If you set a passphrase and forgot it, it cannot be recovered. You will have to start afresh.",
				wrap: 600,
			},
			HBox{
				spacing: 5,
				children: []Widget{
					Label{
						text:   "Passphrase:",
						yAlign: 0.5,
					},
					Entry{
						widgetBase: widgetBase{name: "pw"},
						width:      60,
						password:   true,
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 40},
				children: []Widget{
					Button{
						widgetBase: widgetBase{name: "next"},
						text:       "Next",
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 5},
				children: []Widget{
					Label{
						widgetBase: widgetBase{name: "status"},
					},
				},
			},
		},
	}

	c.ui.Actions() <- SetBoxContents{name: "body", child: ui}
	c.ui.Actions() <- SetFocus{name: "pw"}
	c.ui.Actions() <- UIState{uiStatePassphrase}
	c.ui.Signal()

	for {
		event, ok := <-c.ui.Events()
		if !ok {
			c.ShutdownAndSuspend()
		}

		click, ok := event.(Click)
		if !ok {
			continue
		}
		if click.name != "next" && click.name != "pw" {
			continue
		}

		pw, ok := click.entries["pw"]
		if !ok {
			panic("missing pw")
		}
		if len(pw) == 0 {
			break
		}

		c.ui.Actions() <- Sensitive{name: "next", sensitive: false}
		c.ui.Signal()

		if diskKey, err := c.deriveKey(pw); err != nil {
			panic(err)
		} else {
			copy(c.diskKey[:], diskKey)
		}

		err := c.loadState(state, &c.diskKey)
		if err != badPasswordError {
			return err
		}

		c.ui.Actions() <- SetText{name: "status", text: "Incorrect passphrase or corrupt state file"}
		c.ui.Actions() <- Sensitive{name: "next", sensitive: true}
		c.ui.Signal()
	}

	return nil
}

func (c *client) createPassphraseUI() {
	ui := VBox{
		widgetBase: widgetBase{padding: 40, expand: true, fill: true, name: "vbox"},
		children: []Widget{
			Label{
				widgetBase: widgetBase{font: "DejaVu Sans 30"},
				text:       "Set Passphrase",
			},
			Label{
				widgetBase: widgetBase{
					padding: 20,
					font:    "DejaVu Sans 14",
				},
				text: "Pond keeps private keys, messages etc on disk for a limited amount of time and that information can be encrypted with a passphrase. If you are comfortable with the security of your home directory, this passphrase can be empty and you won't be prompted for it again. If you set a passphrase and forget it, it cannot be recovered. You will have to start afresh.",
				wrap: 600,
			},
			HBox{
				spacing: 5,
				children: []Widget{
					Label{
						text:   "Passphrase:",
						yAlign: 0.5,
					},
					Entry{
						widgetBase: widgetBase{name: "pw"},
						width:      60,
						password:   true,
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 40},
				children: []Widget{
					Button{
						widgetBase: widgetBase{name: "next"},
						text:       "Next",
					},
				},
			},
		},
	}

	c.ui.Actions() <- SetBoxContents{name: "body", child: ui}
	c.ui.Actions() <- SetFocus{name: "pw"}
	c.ui.Actions() <- UIState{uiStateCreatePassphrase}
	c.ui.Signal()

	for {
		event, ok := <-c.ui.Events()
		if !ok {
			c.ShutdownAndSuspend()
		}

		click, ok := event.(Click)
		if !ok {
			continue
		}
		if click.name != "next" && click.name != "pw" {
			continue
		}

		pw, ok := click.entries["pw"]
		if !ok {
			panic("missing pw")
		}
		if len(pw) == 0 {
			break
		}

		c.ui.Actions() <- Sensitive{name: "next", sensitive: false}
		c.ui.Signal()

		c.randBytes(c.diskSalt[:])
		if diskKey, err := c.deriveKey(pw); err != nil {
			panic(err)
		} else {
			copy(c.diskKey[:], diskKey)
		}

		break
	}
}

func (c *client) createAccountUI() {
	defaultServer := "pondserver://ICYUHSAYGIXTKYKXSAHIBWEAQCTEF26WUWEPOVC764WYELCJMUPA@jb644zapje5dvgk3.onion"
	if c.testing {
		defaultServer = "pondserver://PXD4DDBLJD3YCC3EC3DGIYVYZYF5GVZC3T6JFHPUWU2WQ7W3CN5Q@127.0.0.1:16333"
	}

	ui := VBox{
		widgetBase: widgetBase{padding: 40, expand: true, fill: true, name: "vbox"},
		children: []Widget{
			Label{
				widgetBase: widgetBase{font: "DejaVu Sans 30"},
				text:       "Create Account",
			},
			Label{
				widgetBase: widgetBase{
					padding: 20,
					font:    "DejaVu Sans 14",
				},
				text: "In order to use Pond you have to have an account on a server. Servers may set their own account policies, but the default server allows anyone to create an account. If you want to use the default server, just click 'Create'.",
				wrap: 600,
			},
			HBox{
				spacing: 5,
				children: []Widget{
					Label{
						text:   "Server:",
						yAlign: 0.5,
					},
					Entry{
						widgetBase: widgetBase{name: "server"},
						width:      60,
						text:       defaultServer,
					},
				},
			},
			HBox{
				widgetBase: widgetBase{padding: 40},
				children: []Widget{
					Button{
						widgetBase: widgetBase{name: "create"},
						text:       "Create",
					},
				},
			},
		},
	}

	c.ui.Actions() <- SetBoxContents{name: "body", child: ui}
	c.ui.Actions() <- SetFocus{name: "create"}
	c.ui.Actions() <- UIState{uiStateCreateAccount}
	c.ui.Signal()

	var spinnerCreated bool
	for {
		click, ok := <-c.ui.Events()
		if !ok {
			c.ShutdownAndSuspend()
		}
		c.server = click.(Click).entries["server"]

		c.ui.Actions() <- Sensitive{name: "server", sensitive: false}
		c.ui.Actions() <- Sensitive{name: "create", sensitive: false}

		const initialMessage = "Checking..."

		if !spinnerCreated {
			c.ui.Actions() <- Append{
				name: "vbox",
				children: []Widget{
					HBox{
						widgetBase: widgetBase{name: "statusbox"},
						spacing:    10,
						children: []Widget{
							Spinner{
								widgetBase: widgetBase{name: "spinner"},
							},
							Label{
								widgetBase: widgetBase{name: "status"},
								text:       initialMessage,
							},
						},
					},
				},
			}
			spinnerCreated = true
		} else {
			c.ui.Actions() <- StartSpinner{name: "spinner"}
			c.ui.Actions() <- SetText{name: "status", text: initialMessage}
		}
		c.ui.Signal()

		if err := c.doCreateAccount(); err != nil {
			c.ui.Actions() <- StopSpinner{name: "spinner"}
			c.ui.Actions() <- UIError{err}
			c.ui.Actions() <- SetText{name: "status", text: err.Error()}
			c.ui.Actions() <- Sensitive{name: "server", sensitive: true}
			c.ui.Actions() <- Sensitive{name: "create", sensitive: true}
			c.ui.Signal()
			continue
		}

		break
	}
}

func (c *client) ShutdownAndSuspend() {
	if c.writerChan != nil {
		c.save()
	}
	c.Shutdown()
	close(c.ui.Actions())
	select {}
}

func (c *client) Shutdown() {
	if c.writerChan != nil {
		close(c.writerChan)
		<-c.writerDone
	}
	if c.fetchNowChan != nil {
		close(c.fetchNowChan)
	}
}

func NewClient(stateFilename string, ui UI, rand io.Reader, testing, autoFetch bool) *client {
	c := &client{
		testing:         testing,
		autoFetch:       autoFetch,
		stateFilename:   stateFilename,
		log:             NewLog(),
		ui:              ui,
		rand:            rand,
		contacts:        make(map[uint64]*Contact),
		newMessageChan:  make(chan NewMessage),
		messageSentChan: make(chan uint64, 1),
	}
	c.log.toStderr = true

	go c.loadUI()
	return c
}