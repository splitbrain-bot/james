/*{
	"description": "Opens an address in the page the user is looking at. The user is asked first and may say no. Only http and https addresses work.",
	"schema": {
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "the http or https address to open"
			}
		},
		"required": ["url"]
	}
}*/

/**
 * Ask the user and then send the host page to a new address.
 * @param {{url: string}} input the tool input
 * @param {ToolContext} ctx texts and questions of the widget
 * @returns {Promise<{output: string, after: function(): void}>} the answer and the move that follows it
 */
export default async function navigate(input, ctx) {
	let target;
	try {
		target = new URL(String(input.url), location.href);
	} catch {
		throw new Error("the URL is not valid");
	}
	if (target.protocol !== "http:" && target.protocol !== "https:") {
		throw new Error("only http and https URLs are allowed");
	}
	if (!await ctx.confirm(`${ctx.t("widgetConfirmNavigate")}\n\n${target.href}`)) {
		throw new Error("the user declined");
	}
	return {
		output: `navigated to ${target.href}`,
		after: () => {
			location.href = target.href;
		}
	};
}
