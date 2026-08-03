import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { QuestionBox } from './QuestionBox'

const setup = (over: Partial<React.ComponentProps<typeof QuestionBox>> = {}) => {
  const onAsk = vi.fn()
  const onStop = vi.fn()
  render(
    <QuestionBox onAsk={onAsk} onStop={onStop} busy={false} databases={['toy']} {...over} />,
  )
  return { onAsk, onStop }
}

describe('QuestionBox', () => {
  it('asks with the question and the chosen database', () => {
    const { onAsk } = setup({ databases: ['toy', 'demo'] })

    fireEvent.change(screen.getByLabelText('Question'), { target: { value: 'how many orders?' } })
    fireEvent.change(screen.getByLabelText('Database'), { target: { value: 'demo' } })
    fireEvent.click(screen.getByRole('button', { name: 'Ask' }))

    expect(onAsk).toHaveBeenCalledWith('how many orders?', 'demo')
  })

  it('will not submit an empty question', () => {
    const { onAsk } = setup()

    // Disabled rather than submitting and being refused: the reducer and the
    // server both revalidate, so this only saves a pointless round trip.
    expect(screen.getByRole('button', { name: 'Ask' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: 'Ask' }))
    expect(onAsk).not.toHaveBeenCalled()
  })

  it('will not submit whitespace', () => {
    const { onAsk } = setup()

    fireEvent.change(screen.getByLabelText('Question'), { target: { value: '   ' } })
    fireEvent.submit(screen.getByRole('form', { name: 'Ask a question' }))
    expect(onAsk).not.toHaveBeenCalled()
  })

  it('offers a stop while a run is in flight, and refuses a second ask', () => {
    const { onStop } = setup({ busy: true })

    expect(screen.getByRole('button', { name: 'Ask' })).toBeDisabled()
    expect(screen.getByLabelText('Question')).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: 'Stop' }))
    expect(onStop).toHaveBeenCalledOnce()
  })

  it('caps the question at the contract length', () => {
    setup()
    // question_request.v1 caps this at 2000, so the limit is visible in the UI
    // rather than discovered as a rejection.
    expect(screen.getByLabelText('Question')).toHaveAttribute('maxLength', '2000')
  })
})
