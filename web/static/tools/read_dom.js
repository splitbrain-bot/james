/*{
	"description": "Reads the markup of the page the user is looking at, to find out how it is built. Only a few levels below the starting point are shown, deeper elements are replaced by a comment saying how many there are. Read them by calling again with a selector that points at them. Long texts and attribute values are cut off and say how much is missing; set full to get one of them whole.",
	"schema": {
		"type": "object",
		"properties": {
			"selector": {
				"type": "string",
				"description": "CSS selector of the element to start at, the whole page body when it is missing"
			},
			"depth": {
				"type": "integer",
				"minimum": 0,
				"description": "how many levels of elements below the starting point to show, 4 when it is missing"
			},
			"full": {
				"type": "boolean",
				"description": "show texts and attribute values whole instead of cutting off the long ones"
			}
		}
	}
}*/

/** Levels below the starting point shown when the input names none. */
const DEFAULT_DEPTH = 4;

/** Longest text or attribute value shown whole. */
const MAX_VALUE = 500;

/** Largest result. */
const MAX_TEXT = 100000;

/** Elements written without a closing tag. */
const VOID_TAGS = new Set([
	"AREA", "BASE", "BR", "COL", "EMBED", "HR", "IMG", "INPUT", "LINK",
	"META", "PARAM", "SOURCE", "TRACK", "WBR"
]);

/**
 * Read the markup of the host page, or of the part of it under a selector.
 * @param {{selector: ?string, depth: ?number, full: ?boolean}} input the tool input
 * @param {ToolContext} ctx texts and questions of the widget
 * @returns {string} indented markup, cut off where the limits are reached
 */
export default function readDom(input, ctx) {
	let root = document.body;
	if (input.selector) {
		try {
			root = document.querySelector(input.selector);
		} catch {
			throw new Error(`the selector is not valid: ${input.selector}`);
		}
		if (!root) throw new Error(`no element matches the selector: ${input.selector}`);
	}
	const depth = Number.isInteger(input.depth) && input.depth >= 0 ? input.depth : DEFAULT_DEPTH;
	const out = { lines: [], length: 0, cut: false };
	write(out, root, depth, input.full === true, "");
	return out.lines.join("\n");
}

/**
 * Add one line to the result. Once the limit is reached only the closing tags
 * of the elements already opened still follow, so the markup stays balanced.
 * @param {{lines: string[], length: number, cut: boolean}} out the result so far
 * @param {string} line the line to add
 * @param {boolean} closing whether the line closes an element
 */
function push(out, line, closing) {
	if (!out.cut && out.length + line.length + 1 > MAX_TEXT) {
		out.cut = true;
		out.lines.push(`${line.match(/^\s*/)[0]}<!-- cut off here, the limit of ${MAX_TEXT} characters is reached -->`);
	} else if (!out.cut) {
		out.length += line.length + 1;
	}
	if (!out.cut || closing) out.lines.push(line);
}

/**
 * Write one node and what hangs below it.
 * @param {{lines: string[], length: number, cut: boolean}} out the result so far
 * @param {Node} node the node to write
 * @param {number} depth levels of elements still allowed below this node
 * @param {boolean} full whether to show long values whole
 * @param {string} indent the indent of this node
 * @returns {boolean} true once the limit is reached and the walk has to stop
 */
function write(out, node, depth, full, indent) {
	if (node.nodeType === Node.TEXT_NODE) {
		const text = shorten(node.nodeValue.trim(), full);
		push(out, `${indent}${escapeText(text.shown)}${mark(text.rest)}`);
		return out.cut;
	}
	if (node.nodeType === Node.COMMENT_NODE) {
		const text = shorten(node.nodeValue.trim(), full);
		push(out, `${indent}<!-- ${escapeText(text.shown)}${mark(text.rest)} -->`);
		return out.cut;
	}
	if (node.nodeType !== Node.ELEMENT_NODE) return out.cut;

	const tag = node.tagName.toLowerCase();
	const open = `${indent}<${tag}${attributes(node, full)}>`;
	if (VOID_TAGS.has(node.tagName)) {
		push(out, open);
		return out.cut;
	}

	const children = [...node.childNodes].filter(hasContent);
	if (children.length === 0) {
		push(out, `${open}</${tag}>`);
		return out.cut;
	}
	if (children.length === 1 && children[0].nodeType === Node.TEXT_NODE) {
		const text = shorten(children[0].nodeValue.trim(), full);
		push(out, `${open}${escapeText(text.shown)}${mark(text.rest)}</${tag}>`);
		return out.cut;
	}

	push(out, open);
	if (out.cut) return true;
	const inner = `${indent}  `;
	const hidden = depth === 0 ? children.filter((child) => child.nodeType === Node.ELEMENT_NODE) : [];
	for (const child of children) {
		if (depth === 0 && child.nodeType === Node.ELEMENT_NODE) continue;
		if (write(out, child, depth - 1, full, inner)) break;
	}
	if (!out.cut && hidden.length > 0) {
		push(out, `${inner}<!-- ${hidden.length} more element${hidden.length === 1 ? "" : "s"} -->`);
	}
	push(out, `${indent}</${tag}>`, true);
	return out.cut;
}

/**
 * Tell whether a node carries anything worth writing.
 * @param {Node} node the node to judge
 * @returns {boolean} true for elements and for texts and comments that are not blank
 */
function hasContent(node) {
	if (node.nodeType === Node.ELEMENT_NODE) return true;
	if (node.nodeType === Node.TEXT_NODE || node.nodeType === Node.COMMENT_NODE) {
		return node.nodeValue.trim() !== "";
	}
	return false;
}

/**
 * Write the attributes of an element, the long values cut off.
 * @param {Element} node the element to read
 * @param {boolean} full whether to show long values whole
 * @returns {string} the attributes, each one led by a space
 */
function attributes(node, full) {
	let text = "";
	for (const attr of node.attributes) {
		const value = shorten(attr.value, full);
		text += ` ${attr.name}="${escapeValue(value.shown)}"${mark(value.rest)}`;
	}
	return text;
}

/**
 * Cut a long value down to the limit, with an ellipsis where it was cut.
 * @param {string} value the value to shorten
 * @param {boolean} full whether to return it whole
 * @returns {{shown: string, rest: number}} the part to show and how many characters are left out
 */
function shorten(value, full) {
	if (full || value.length <= MAX_VALUE) return { shown: value, rest: 0 };
	return { shown: `${value.slice(0, MAX_VALUE)}…`, rest: value.length - MAX_VALUE };
}

/**
 * Name the amount a cut left out.
 * @param {number} rest how many characters are left out
 * @returns {string} the note to append, empty when nothing was cut
 */
function mark(rest) {
	return rest > 0 ? ` [${rest} more characters]` : "";
}

/**
 * Escape a text so it cannot be read as markup.
 * @param {string} text the raw text
 * @returns {string} the escaped text
 */
function escapeText(text) {
	return text.replace(/&/g, "&amp;").replace(/</g, "&lt;");
}

/**
 * Escape an attribute value so it cannot end its quotes.
 * @param {string} value the raw value
 * @returns {string} the escaped value
 */
function escapeValue(value) {
	return value.replace(/&/g, "&amp;").replace(/"/g, "&quot;");
}
