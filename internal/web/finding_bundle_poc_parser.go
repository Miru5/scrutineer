package web

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Run before Goldmark's standard fenced block parser (priority 700).
const legacyPoCFencePriority = 699

var pocMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithBlockParsers(util.Prioritized(
		&legacyPoCFenceParser{BlockParser: parser.NewFencedCodeBlockParser()}, legacyPoCFencePriority,
	))),
)

type legacyPoCFenceParser struct {
	parser.BlockParser
}

//nolint:ireturn // Required by Goldmark's BlockParser interface.
func (p *legacyPoCFenceParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || !bytes.HasPrefix(line[pos:], []byte("```")) || bytes.HasPrefix(line[pos:], []byte("````")) {
		return nil, parser.NoChildren
	}
	for field := range strings.FieldsSeq(string(line[pos+3:])) {
		if strings.HasPrefix(field, "filename=") {
			return nil, parser.NoChildren
		}
	}
	return p.BlockParser.Open(parent, reader, pc)
}

func (p *legacyPoCFenceParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	line, segment := reader.PeekLine()
	state := p.BlockParser.Continue(node, reader, pc)
	if state == parser.Close {
		return state
	}
	line = bytes.TrimRight(line, " \t\r\n")
	body, closed := bytes.CutSuffix(line, []byte("```"))
	if !closed || len(bytes.TrimSpace(body)) == 0 || body[len(body)-1] == '`' {
		return state
	}
	// Older reports glued the closing fence to the last content line.
	last := node.Lines().Len() - 1
	content := node.Lines().At(last)
	content.Stop = segment.Start + len(body)
	node.Lines().Set(last, content)
	return parser.Close
}
