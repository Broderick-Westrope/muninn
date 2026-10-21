package web

import "github.com/alecthomas/chroma/v2"

// anvilNight ports the token colors of the Anvil Night editor theme (Tokyo
// Night on a near-black #050014 background) so highlighted files match the
// editor and terminal. Chroma's bundled tokyonight-night differs in enough
// places (comments, types, operators) to look noticeably off beside them.
var anvilNight = chroma.MustNewStyle("anvil-night", chroma.StyleEntries{
	chroma.Background:       "bg:#050014 #c0caf5",
	chroma.LineNumbers:      "#3b4261",
	chroma.LineNumbersTable: "#3b4261",
	chroma.LineHighlight:    "bg:#1a1b26",
	chroma.Error:            "#db4b4b",
	chroma.Other:            "#c0caf5",

	chroma.Comment:         "italic #565f89",
	chroma.CommentPreproc:  "noitalic #7dcfff",
	chroma.CommentHashbang: "italic #565f89",

	chroma.Keyword:         "italic #bb9af7",
	chroma.KeywordConstant: "noitalic #ff9e64",
	chroma.KeywordType:     "noitalic #2ac3de",

	chroma.Operator:     "#89ddff",
	chroma.OperatorWord: "italic #bb9af7",
	chroma.Punctuation:  "#a9b1d6",

	chroma.Name:                 "#c0caf5",
	chroma.NameAttribute:        "#bb9af7",
	chroma.NameBuiltin:          "#2ac3de",
	chroma.NameBuiltinPseudo:    "#f7768e",
	chroma.NameClass:            "#2ac3de",
	chroma.NameConstant:         "#ff9e64",
	chroma.NameDecorator:        "#7aa2f7",
	chroma.NameEntity:           "#7dcfff",
	chroma.NameException:        "#2ac3de",
	chroma.NameFunction:         "#7aa2f7",
	chroma.NameLabel:            "#7aa2f7",
	chroma.NameNamespace:        "#7dcfff",
	chroma.NameProperty:         "#73daca",
	chroma.NameTag:              "#f7768e",
	chroma.NameVariable:         "#c0caf5",
	chroma.NameVariableInstance: "#73daca",

	chroma.LiteralString:       "#9ece6a",
	chroma.LiteralStringEscape: "#bb9af7",
	chroma.LiteralStringRegex:  "#b4f9f8",
	chroma.LiteralStringSymbol: "#73daca",
	chroma.LiteralNumber:       "#ff9e64",

	chroma.GenericDeleted:    "#f7768e",
	chroma.GenericEmph:       "italic #bb9af7",
	chroma.GenericError:      "#db4b4b",
	chroma.GenericHeading:    "bold #7aa2f7",
	chroma.GenericInserted:   "#9ece6a",
	chroma.GenericOutput:     "#a9b1d6",
	chroma.GenericPrompt:     "#565f89",
	chroma.GenericStrong:     "bold #ff9e64",
	chroma.GenericSubheading: "bold #7aa2f7",
	chroma.GenericTraceback:  "#db4b4b",
	chroma.GenericUnderline:  "underline",
})
