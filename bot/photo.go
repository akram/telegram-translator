package bot

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// Flag colors: each flag is defined as vertical stripes (left to right)
var flagColors = map[string][][3]uint8{
	"AR": {{0, 150, 57}, {255, 255, 255}, {206, 17, 38}},     // UAE
	"BG": {{255, 255, 255}, {0, 150, 110}, {214, 38, 18}},     // Bulgaria
	"CS": {{255, 255, 255}, {214, 38, 18}, {0, 42, 106}},      // Czech (simplified)
	"DA": {{198, 12, 48}, {255, 255, 255}, {198, 12, 48}},     // Denmark (simplified)
	"DE": {{0, 0, 0}, {221, 0, 0}, {255, 206, 0}},             // Germany (horizontal→vertical approx)
	"EL": {{13, 94, 175}, {255, 255, 255}, {13, 94, 175}},     // Greece (simplified)
	"EN": {{0, 36, 125}, {255, 255, 255}, {207, 20, 43}},      // UK (simplified)
	"ES": {{198, 11, 30}, {255, 196, 0}, {198, 11, 30}},       // Spain
	"ET": {{0, 114, 206}, {0, 0, 0}, {255, 255, 255}},         // Estonia
	"FI": {{255, 255, 255}, {0, 47, 108}, {255, 255, 255}},    // Finland
	"FR": {{0, 35, 149}, {255, 255, 255}, {237, 41, 57}},      // France
	"HU": {{205, 42, 62}, {255, 255, 255}, {67, 111, 77}},     // Hungary
	"ID": {{255, 0, 0}, {255, 255, 255}, {255, 0, 0}},         // Indonesia
	"IT": {{0, 140, 69}, {255, 255, 255}, {205, 33, 42}},      // Italy
	"JA": {{255, 255, 255}, {188, 0, 45}, {255, 255, 255}},    // Japan (simplified)
	"KO": {{255, 255, 255}, {205, 46, 58}, {255, 255, 255}},   // Korea (simplified)
	"LT": {{253, 185, 19}, {0, 106, 68}, {190, 37, 37}},       // Lithuania
	"LV": {{158, 48, 57}, {255, 255, 255}, {158, 48, 57}},     // Latvia
	"NB": {{186, 12, 47}, {255, 255, 255}, {0, 32, 91}},       // Norway
	"NL": {{174, 28, 40}, {255, 255, 255}, {33, 70, 139}},     // Netherlands
	"PL": {{255, 255, 255}, {255, 255, 255}, {220, 20, 60}},   // Poland
	"PT": {{0, 102, 0}, {255, 0, 0}, {255, 0, 0}},             // Portugal
	"RO": {{0, 43, 127}, {252, 209, 22}, {206, 17, 38}},       // Romania
	"RU": {{255, 255, 255}, {0, 57, 166}, {213, 43, 30}},      // Russia
	"SK": {{255, 255, 255}, {11, 78, 162}, {238, 28, 37}},     // Slovakia
	"SL": {{255, 255, 255}, {0, 51, 160}, {237, 28, 36}},      // Slovenia
	"SV": {{0, 106, 167}, {254, 204, 2}, {0, 106, 167}},       // Sweden
	"TR": {{227, 10, 23}, {255, 255, 255}, {227, 10, 23}},     // Turkey
	"UK": {{0, 87, 183}, {0, 87, 183}, {255, 215, 0}},         // Ukraine
	"ZH": {{238, 28, 37}, {238, 28, 37}, {255, 255, 0}},       // China (simplified)
}

// SyncGroupPhoto downloads the source group photo, overlays a flag badge, and sets it on the destination.
func (b *Bot) SyncGroupPhoto(ctx context.Context, api *tg.Client) error {
	srcPeer, err := b.getPeer(b.cfg.Channels.Sources[0])
	if err != nil {
		return err
	}
	srcInputPeer := srcPeer.(*tg.InputPeerChannel)
	srcChannel := &tg.InputChannel{ChannelID: srcInputPeer.ChannelID, AccessHash: srcInputPeer.AccessHash}

	// Get full channel info to get the photo
	fullChat, err := api.ChannelsGetFullChannel(ctx, srcChannel)
	if err != nil {
		return fmt.Errorf("getting source channel info: %w", err)
	}

	// Find photo from chats
	for _, chat := range fullChat.Chats {
		if ch, ok := chat.(*tg.Channel); ok && ch.ID == srcInputPeer.ChannelID {
			if p, ok := ch.Photo.(*tg.ChatPhoto); ok {
				photoLoc := &tg.InputPeerPhotoFileLocation{
					Peer:    srcPeer,
					Big:     true,
					PhotoID: p.PhotoID,
				}

				var buf bytes.Buffer
				dl := downloader.NewDownloader()
				_, err := dl.Download(api, photoLoc).Stream(ctx, &buf)
				if err != nil {
					return fmt.Errorf("downloading source photo: %w", err)
				}

				// Decode, overlay flag, re-encode
				img, _, err := image.Decode(&buf)
				if err != nil {
					return fmt.Errorf("decoding source photo: %w", err)
				}

				result := overlayFlag(img, b.cfg.Languages.TargetLang)

				var outBuf bytes.Buffer
				if err := jpeg.Encode(&outBuf, result, &jpeg.Options{Quality: 95}); err != nil {
					return fmt.Errorf("encoding result photo: %w", err)
				}

				// Upload the new photo
				ul := uploader.NewUploader(api)
				inputFile, err := ul.FromBytes(ctx, "photo.jpg", outBuf.Bytes())
				if err != nil {
					return fmt.Errorf("uploading photo: %w", err)
				}

				// Set on destination
				dstPeer, err := b.getPeer(b.cfg.Channels.Destination)
				if err != nil {
					return err
				}
				dstInputPeer := dstPeer.(*tg.InputPeerChannel)
				dstChannel := &tg.InputChannel{ChannelID: dstInputPeer.ChannelID, AccessHash: dstInputPeer.AccessHash}

				_, err = api.ChannelsEditPhoto(ctx, &tg.ChannelsEditPhotoRequest{
					Channel: dstChannel,
					Photo: &tg.InputChatUploadedPhoto{
						File: inputFile,
					},
				})
				if err != nil {
					return fmt.Errorf("setting destination photo: %w", err)
				}

				b.logger.Info("synced group photo with flag overlay")
				return nil
			}
		}
	}

	b.logger.Debug("source channel has no photo")
	return nil
}

// overlayFlag draws a circular flag badge in the bottom-right corner of the image.
func overlayFlag(src image.Image, lang string) image.Image {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)

	stripes, ok := flagColors[lang]
	if !ok {
		return dst
	}

	w := bounds.Dx()
	h := bounds.Dy()

	// Badge: circle in bottom-right, diameter = 30% of image
	diameter := int(float64(w) * 0.30)
	radius := diameter / 2
	centerX := w - radius - int(float64(w)*0.05)
	centerY := h - radius - int(float64(h)*0.05)

	// Draw white border circle (slightly larger)
	borderRadius := radius + int(float64(radius)*0.1)
	drawCircle(dst, centerX, centerY, borderRadius, color.White)

	// Draw flag stripes within the circle
	stripeWidth := diameter / len(stripes)
	for i, stripe := range stripes {
		c := color.RGBA{stripe[0], stripe[1], stripe[2], 255}
		x0 := centerX - radius + i*stripeWidth
		x1 := x0 + stripeWidth
		if i == len(stripes)-1 {
			x1 = centerX + radius
		}
		drawCircleStripe(dst, centerX, centerY, radius, x0, x1, c)
	}

	return dst
}

func drawCircle(dst *image.RGBA, cx, cy, r int, c color.Color) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx := float64(x - cx)
			dy := float64(y - cy)
			if math.Sqrt(dx*dx+dy*dy) <= float64(r) {
				if x >= 0 && y >= 0 && x < dst.Bounds().Dx() && y < dst.Bounds().Dy() {
					dst.Set(x, y, c)
				}
			}
		}
	}
}

func drawCircleStripe(dst *image.RGBA, cx, cy, r, x0, x1 int, c color.Color) {
	for y := cy - r; y <= cy+r; y++ {
		for x := x0; x < x1; x++ {
			dx := float64(x - cx)
			dy := float64(y - cy)
			if math.Sqrt(dx*dx+dy*dy) <= float64(r) {
				if x >= 0 && y >= 0 && x < dst.Bounds().Dx() && y < dst.Bounds().Dy() {
					dst.Set(x, y, c)
				}
			}
		}
	}
}
