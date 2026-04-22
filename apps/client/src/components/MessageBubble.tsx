import { Message, DeliveryState } from '../types';

interface Props {
  message: Message;
  isMine: boolean;
  senderName?: string;
}

function formatTime(d: Date) {
  return new Date(d).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function StateIcon({ state }: { state: DeliveryState }) {
  if (state === 0) return <span className="text-white/50 text-[10px] leading-none">◷</span>;
  if (state === 1) return (
    <svg className="w-3 h-3 text-white/70" viewBox="0 0 16 16" fill="currentColor">
      <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
    </svg>
  );
  if (state === 2) return (
    <span className="inline-flex -space-x-1.5">
      <svg className="w-3 h-3 text-white/70" viewBox="0 0 16 16" fill="currentColor">
        <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
      </svg>
      <svg className="w-3 h-3 text-white/70" viewBox="0 0 16 16" fill="currentColor">
        <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
      </svg>
    </span>
  );
  if (state >= 3) return (
    <span className="inline-flex -space-x-1.5">
      <svg className="w-3 h-3 text-white" viewBox="0 0 16 16" fill="currentColor">
        <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
      </svg>
      <svg className="w-3 h-3 text-white" viewBox="0 0 16 16" fill="currentColor">
        <path d="M13.854 3.646a.5.5 0 010 .708l-7 7a.5.5 0 01-.708 0l-3.5-3.5a.5.5 0 11.708-.708L6.5 10.293l6.646-6.647a.5.5 0 01.708 0z"/>
      </svg>
    </span>
  );
  return null;
}

const stateLabel: Record<number, string> = {
  0: 'pending',
  1: 'sent',
  2: 'delivered',
  3: 'read',
  4: 'failed',
};

export default function MessageBubble({ message, isMine, senderName }: Props) {
  return (
    <div className={`flex ${isMine ? 'justify-end' : 'justify-start'} animate-fade-in`}>
      <div className={`max-w-xs lg:max-w-sm xl:max-w-md ${isMine ? 'items-end' : 'items-start'} flex flex-col`}>
        {/* Sender name for group chats */}
        {!isMine && senderName && (
          <span className="text-[10px] font-medium text-indigo-500 px-3 mb-0.5">{senderName}</span>
        )}
        <div
          className={`px-4 py-2.5 rounded-2xl shadow-sm ${
            isMine
              ? 'bg-indigo-600 text-white rounded-br-sm'
              : 'bg-white text-gray-900 rounded-bl-sm border border-gray-100'
          }`}
        >
          <p className="text-sm leading-relaxed whitespace-pre-wrap break-words">{message.content}</p>
          <div className={`flex items-center gap-1 mt-1 ${isMine ? 'justify-end' : 'justify-end'}`}>
            <span className={`text-[10px] ${isMine ? 'text-white/60' : 'text-gray-400'}`}>
              {formatTime(message.timestamp)}
            </span>
            {isMine && <StateIcon state={message.state} />}
          </div>
        </div>
        {/* State badge (dev helper) */}
        {isMine && (
          <span className="text-[9px] text-gray-300 px-3 mt-0.5 capitalize">
            {stateLabel[message.state] ?? '?'}
          </span>
        )}
      </div>
    </div>
  );
}
