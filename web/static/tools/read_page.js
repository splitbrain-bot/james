/*{
	"description": "Reads the page the user is looking at right now: its address, its title and its text. Give a CSS selector to read only one part of the page.",
	"schema": {
		"type": "object",
		"properties": {
			"selector": {
				"type": "string",
				"description": "CSS selector of the part to read, the whole page when it is missing"
			}
		}
	}
}*/

/** Largest result, the URL and the title included. */
const MAX_TEXT = 100000;

/**
 * Read the host page, or the part of it under a selector.
 * @param {{selector: ?string}} input the tool input
 * @param {ToolContext} ctx texts and dialogs of the widget
 * @returns {string} URL, title and rendered text of the page
 */
export default function readPage(input, ctx) {
	let root = document.body;
	if (input.selector) {
		try {
			root = document.querySelector(input.selector);
		} catch {
			throw new Error(`the selector is not valid: ${input.selector}`);
		}
		if (!root) throw new Error(`no element matches the selector: ${input.selector}`);
		if (!root.checkVisibility()) throw new Error(`the element is not shown: ${input.selector}`);
	}
	const text = `URL: ${location.href}\nTitle: ${document.title}\n\n${root.innerText}`;
	return text.slice(0, MAX_TEXT);
}
