import { CHARS_PER_TOKEN, MAX_AVAILABLE_PROMPT_LENGTH } from '../../prompt/constants'
import { Message } from '../../sourcegraph-api'
import { getShortTimestamp } from '../../timestamp'

import { Interaction } from './interaction'
import { ChatMessage } from './messages'

export class Transcript {
    private interactions: Interaction[] = []

    public addInteraction(interaction: Interaction | null): void {
        if (!interaction) {
            return
        }
        this.interactions.push(interaction)
    }

    private getLastInteraction(): Interaction | null {
        return this.interactions.length > 0 ? this.interactions[this.interactions.length - 1] : null
    }

    public addAssistantResponse(text: string): void {
        this.getLastInteraction()?.setAssistantMessage({
            speaker: 'assistant',
            text,
            displayText: text,
            timestamp: getShortTimestamp(),
        })
    }

    private async getLastInteractionWithContextIndex(): Promise<number> {
        for (let index = this.interactions.length - 1; index >= 0; index--) {
            const hasContext = await this.interactions[index].hasContext()
            if (hasContext) {
                return index
            }
        }
        return -1
    }

    public async toPrompt(preamble: Message[] = []): Promise<Message[]> {
        const lastInteractionWithContextIndex = await this.getLastInteractionWithContextIndex()
        const messages: Message[] = []
        for (let index = 0; index < this.interactions.length; index++) {
            // Include context messages for the last interaction that has a non-empty context.
            const interactionMessages = await this.interactions[index].toPrompt(
                index === lastInteractionWithContextIndex
            )
            messages.push(...interactionMessages)
        }

        const preambleTokensUsage = preamble.reduce((acc, message) => acc + estimateTokensUsage(message), 0)
        const truncatedMessages = truncatePrompt(messages, MAX_AVAILABLE_PROMPT_LENGTH - preambleTokensUsage)
        return [...preamble, ...truncatedMessages]
    }

    public toChat(): ChatMessage[] {
        return this.interactions.flatMap(interaction => interaction.toChat())
    }

    public reset(): void {
        this.interactions = []
    }
}

function truncatePrompt(messages: Message[], maxTokens: number): Message[] {
    const newPromptMessages = []
    let availablePromptTokensBudget = maxTokens
    for (let i = messages.length - 1; i >= 1; i -= 2) {
        const humanMessage = messages[i - 1]
        const botMessage = messages[i]
        const combinedTokensUsage = estimateTokensUsage(humanMessage) + estimateTokensUsage(botMessage)

        // We stop adding pairs of messages once we exceed the available tokens budget.
        if (combinedTokensUsage <= availablePromptTokensBudget) {
            newPromptMessages.push(botMessage, humanMessage)
            availablePromptTokensBudget -= combinedTokensUsage
        } else {
            break
        }
    }

    // Reverse the prompt messages, so they appear in chat order (older -> newer).
    return newPromptMessages.reverse()
}

function estimateTokensUsage(message: Message): number {
    return Math.round(message.text.length / CHARS_PER_TOKEN)
}
