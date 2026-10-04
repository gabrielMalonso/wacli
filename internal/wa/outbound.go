package wa

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// OutboundClient is the durable dispatch boundary. SendOutbound invokes the SDK
// once; the SDK retains its normal frame and retry-receipt recovery policy.
type OutboundClient interface {
	GenerateOutboundMessageID() (string, error)
	SendOutbound(context.Context, types.JID, string, *waE2E.Message) (whatsmeow.SendResponse, error)
	Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	LinkedJID() string
	LinkedLID() string
}

func (c *Client) GenerateOutboundMessageID() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return "", fmt.Errorf("whatsapp client is not initialized")
	}
	return c.client.GenerateMessageID(), nil
}

func (c *Client) SendOutbound(ctx context.Context, to types.JID, id string, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || id == "" || msg == nil {
		return whatsmeow.SendResponse{}, fmt.Errorf("invalid outbound SDK invocation")
	}
	return cli.SendMessage(ctx, to, msg, whatsmeow.SendRequestExtra{ID: id})
}
