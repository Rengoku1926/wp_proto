package handler

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

//each connected user gets the Client object 
//through this he can read messages from the socket (readpump)
//send messages via a channel (writepump)
//stores messages in db
//routes messages to other connected users
//sends ack

type Client struct {
	conn *websocket.Conn //actual ws connection
	userID uuid.UUID //authenticated userID
	send chan []byte //send is a channel for outgoing messages
	msgRepo *repository.MessageRepo //db layer to save/update messages
	registry *ConnRegistry //global map of active uses (userID -> Client)
}

//incoming messages to read by the user
func (c *Client) readPump(){
	//when the connection closes remove the user from the active conn and close the socket
	defer func(){
		c.registry.Remove(c.userID)
		c.conn.Close()
	}()

	//infinite read loop
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil{
			//logs unexpected ws disconnect
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure){
				log.Printf("ws read error for %s: %v", c.userID, err)
			}
			return 
		}

		// protocol wraps messages like { "type":"message", payload: {...}}
		var env model.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			log.Printf("Bad envelope from %s: %v", c.userID, err)
			continue
		}

		switch env.Type{
		//switch on message type 
		//makes system extensible : eg - "typing", "read_reciept"
		case "message":
			c.handleMessage(env.Payload)
		default: 
			log.Printf("unknown envelope type %q from %s", env.Type, c.userID)
		}
	}
}

//core chat logic
//this function is used as Client.handleMessage()
func (c *Client) handleMessage(payload json.RawMessage){
	//decode the message
	var msg model.Message
	if err := json.Unmarshal(payload, &msg); err != nil {
		log.Printf("Bad message payload from %s: %v", c.userID, err)
		return 
	}

	//c.userID is the authenticated user from db
	//if front end sends = "victim-id" as SenderID
	//force sender to the auth user from the db that is c.userID
	msg.SenderID = c.userID
	msg.State = model.StatePending

	//saved the message to db
	ctx := context.Background()
	saved, err := c.msgRepo.Create(ctx, &msg)
	if err != nil {
		log.Printf("Failed to save message : %v", err)
		return 
	}

	//finds the recipient
	recipient := c.registry.Get(saved.RecipientID)
	//if recipient is found, send it to him if online
	if recipient != nil {
		//wrap the saved message back into an envelope for the recipient
		outPayload, _ := json.Marshal(saved)
		outEnv, _ := json.Marshal(model.Envelope{
			Type: "message",
			Payload: outPayload,
		})

		select {
		//this case tries to send outEnv(message bytes) into recipient.sed(recipient's outgoing channel)
		
		case recipient.send <- outEnv:
			//mark as sent since it reached recipient's buffer
			_ = c.msgRepo.UpdateState(ctx, saved.ID, model.StateSent)
		default:
			log.Printf("recipient %s send buffer full, dropping", saved.RecipientID)
		}
	}

	ackPayload, _ := json.Marshal(model.ACK{MessageId: saved.ID})
	ackEnv, _ := json.Marshal(model.Envelope{
		Type: "ack",
		Payload: ackPayload,
	})
	select {
	case c.send <- ackEnv:
	default:
		log.Printf("sender %s send buffer full, dropping ack ", c.userID)
	}
}

//writepump pumps messages from the send channel to the websocket. 
//it runs its own goroutine - one per connection
func(c *Client) writePump(){
	defer c.conn.Close()

	for msg := range c.send{
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil{
			log.Printf("ws write error for %s: %v", c.userID, err)
			return
		}
	}
}	