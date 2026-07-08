package main

import (
	"fmt"
	"os"
	"strings"
)

func assert(condition bool, message string) {
	if !condition {
		fmt.Printf("❌ ASSERTION FAILED: %s\n", message)
		os.Exit(1)
	}
}

func main() {
	fmt.Println("=== RUNNING SUPERTONIC TEXT CHUNKER TESTS ===")

	// 1. Test space-less sentence boundary splitting with Devanagari full stops (। and ॥)
	hindiText := "नमस्ते।मेरा नाम आलोक है॥क्या सब ठीक है?"
	sentences := splitSentences(hindiText)
	fmt.Printf("Hindi sentences split: %q\n", sentences)
	assert(len(sentences) == 3, "Expected Hindi text to split into 3 sentences")
	assert(strings.TrimSpace(sentences[0]) == "नमस्ते।", "Expected 'नमस्ते।'")
	assert(strings.TrimSpace(sentences[1]) == "मेरा नाम आलोक है॥", "Expected 'मेरा नाम आलोक है॥'")
	assert(strings.TrimSpace(sentences[2]) == "क्या सब ठीक है?", "Expected 'क्या सब ठीक है?'")
	fmt.Println("✅ 1. Space-less Hindi full-stop splitting passed.")

	// 2. Test space-less sentence boundary splitting with Japanese full stops (。)
	japaneseText := "こんにちは。お元気ですか！私は元気です？"
	jaSentences := splitSentences(japaneseText)
	fmt.Printf("Japanese sentences split: %q\n", jaSentences)
	assert(len(jaSentences) == 3, "Expected Japanese text to split into 3 sentences")
	assert(strings.TrimSpace(jaSentences[0]) == "こんにちは。", "Expected 'こんにちは。'")
	assert(strings.TrimSpace(jaSentences[1]) == "お元気ですか！", "Expected 'お元気ですか！'")
	assert(strings.TrimSpace(jaSentences[2]) == "私は元気です？", "Expected '私は元気です？'")
	fmt.Println("✅ 2. Space-less Japanese full-stop splitting passed.")

	// 3. Test multi-byte abbreviation parsing safety (should not panic on Devanagari full stop)
	// (Under the hood, this executes splitSentences without crashing or index boundaries panic)
	panicCheckText := "नमस्ते। डा. आलोक बहुत अच्छे हैं।"
	safeSentences := splitSentences(panicCheckText)
	fmt.Printf("Safe abbreviation parse output: %q\n", safeSentences)
	assert(len(safeSentences) == 2, "Expected text to split into 2 sentences safely")
	fmt.Println("✅ 3. Multi-byte abbreviation safe parsing passed (no panics).")

	// 4. Test character/rune-based chunking limits
	// Hindi text containing 110 characters (which is ~330 bytes)
	// Byte-based chunker would have split this aggressively on commas, but rune-based chunker should keep it as 1 chunk.
	longHindiString := "यह एक बहुत ही लंबी हिंदी वाक्य है, जिसमें कई सारे शब्द हैं, और यह कुल मिलाकर लगभग एक सौ दस अक्षरों की है।"
	// Let's verify the rune length of this string
	runeCount := len([]rune(longHindiString))
	fmt.Printf("Long Hindi string rune count: %d, byte count: %d\n", runeCount, len(longHindiString))
	assert(runeCount < 300, "Rune count must be under 300")
	assert(len(longHindiString) > 300, "Byte length must be over 300 to prove the bug existed")

	chunks := chunkText(longHindiString, 300)
	fmt.Printf("Hindi chunks generated (maxLen 300): %q\n", chunks)
	assert(len(chunks) == 1, "Rune-based chunker should have kept under-300 rune text in a single chunk")
	fmt.Println("✅ 4. Rune-based chunking limits passed (no premature splits for Hindi).")

	// 5. Test comma-preservation in forced splits
	// Create a text that exceeds maxLen (e.g. maxLen 30) and contains commas
	commaText := "यह एक मध्यम वाक्य है, जिसमें अल्पविराम हैं, और यह सीमा पार करता है।"
	commaChunks := chunkText(commaText, 25)
	fmt.Printf("Comma chunks generated (maxLen 25): %q\n", commaChunks)
	assert(len(commaChunks) > 1, "Expected text to be split into multiple chunks")
	
	// Verify that the flushed chunk ends with a comma ',' so preprocessText doesn't append a hard '.'
	firstChunkEndsWithComma := strings.HasSuffix(commaChunks[0], ",")
	fmt.Printf("First chunk ends with comma: %t (Content: %q)\n", firstChunkEndsWithComma, commaChunks[0])
	assert(firstChunkEndsWithComma, "First chunk must end with a comma to preserve inflections and prevent forced periods")
	
	// Test preprocessing of first chunk
	preprocessed := preprocessText(commaChunks[0], "hi")
	fmt.Printf("Preprocessed first chunk: %q\n", preprocessed)
	assert(!strings.Contains(preprocessed, "."), "Preprocessed first chunk must NOT contain a period because it ends with a comma")
	fmt.Println("✅ 5. Comma-preservation and period avoidance passed.")

	// 6. Test fallback hard-slicing on space-less / comma-less long blocks (Japanese)
	// Japanese block with 80 characters (no spaces, no commas) with a maxLen of 30.
	longJapaneseWord := "これは非常に長い日本語の文章でありスペースもコンマも一切含まれていないため通常のチャンカーでは分割できないはずのテキストです"
	jaWordChunks := chunkText(longJapaneseWord, 30)
	fmt.Printf("Japanese hard-sliced chunks (maxLen 30): %q\n", jaWordChunks)
	assert(len(jaWordChunks) == 3, "Expected Japanese block to be split into 3 chunks")
	for _, c := range jaWordChunks {
		runesInChunk := len([]rune(c))
		fmt.Printf("  Chunk: %q (rune count: %d)\n", c, runesInChunk)
		assert(runesInChunk <= 30, "Each sliced chunk must be strictly under maxLen")
	}
	fmt.Println("✅ 6. Space-less Japanese hard-slicing fallback passed.")

	fmt.Println("\n🎉 ALL SUPERTONIC TEXT CHUNKER TESTS PASSED SUCCESSFULLY! 🎉")
}
