# Documentação

- `adr/`: decisões de arquitetura; índice em [ARCHITECTURE.md](../ARCHITECTURE.md).
- `guias/`: operação e diagnóstico.
- `testes/`: cobertura, comandos e evidências dos cenários de integração.
- [Escopo e limites operacionais](../ARCHITECTURE.md#escopo-e-limites-operacionais).

## Guias

- [Execução, configuração, contratos e migrations](guias/operacao.md)
- [Observabilidade](guias/observabilidade.md)

## Testes

- [Evidências de integração e reprodução](testes/evidencias-entrega.md)

- [Carga e custo das validações financeiras](testes/carga.md)
- [Otimização das validações e comparação entre versões](testes/otimizacao-validacoes.md)
- [Matriz de reversões e referências](testes/matriz-reversoes.md)
- [Redrive e DLQ](testes/redrive-dlq.md)
- [Recuperação do consumidor](testes/recuperacao-consumidor.md)
- [Recuperação do publisher](testes/recuperacao-publisher.md)
- [Recuperação após falha de banco](testes/recuperacao-banco-dlq.md)
- [Recuperação com três processos](testes/recuperacao-tres-processos.md)
