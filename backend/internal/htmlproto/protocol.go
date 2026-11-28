package htmlproto

const (
	MIME           = "text/html"
	InspectProfile = "html"
	ConvertProfile = "html-transform"
	HeaderBytes    = 4
	MaxInput       = 4 << 20
	MaxOutput      = 32 << 20
	Batch          = 10
)

type Layout struct {
	Letter, Landscape bool
	Margin            byte
}

func Parse(header [HeaderBytes]byte) (Layout, bool) {
	o := Layout{header[1] == 1, header[2] == 1, header[3]}
	return o, header[0] == 'H' && header[1] <= 1 && header[2] <= 1 && o.Margin >= 5 && o.Margin <= 30
}

func (o Layout) Header() []byte {
	header := []byte{'H', 0, 0, o.Margin}
	if o.Letter {
		header[1] = 1
	}
	if o.Landscape {
		header[2] = 1
	}
	return header
}
